/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
)

// Defaults of the scheduler wait (plan 18, "Waiting for the scheduler").
const (
	DefaultScheduleWait = 45 * time.Second
	DefaultPollInterval = 2 * time.Second
)

// CodeErrorServerLeft is the code of a create whose server went to ERROR and
// could not be deleted.
const CodeErrorServerLeft = "ErrorServerNotDeleted"

// WaitOptions bound CreateAndWait's polling; zero fields are the defaults.
type WaitOptions struct {
	Timeout  time.Duration
	Interval time.Duration
}

// CreateAndWait creates the server and polls it until the scheduler is done:
// the task state leaves "scheduling" or the status is ERROR, at most
// Timeout. Nova answers 202 before a host is chosen and a capacity failure
// shows seconds later as ERROR with a fault, so waiting here lets the attempt
// loop try the next candidate.
//
// A server in ERROR is deleted before the fault's classification is returned,
// so no broken server keeps the idempotency key (a server being deleted is
// skipped by FindByKey). If that delete fails, the error is Retryable: the
// core's retry finds the server through FindByKey's stale list and deletes it
// again. A server still scheduling at the timeout, or one whose poll fails, is
// returned as it was last seen, without an error: the ID is never lost and the
// rest of the boot is the core's launch timeout.
//
// The driver's CreateInstance is FindByKey (replay), BuildCreateRequest, then this.
func CreateAndWait(
	ctx context.Context, c osapi.Compute, req osapi.CreateServerRequest, o WaitOptions,
) (*osapi.Server, error) {
	if o.Timeout <= 0 {
		o.Timeout = DefaultScheduleWait
	}
	if o.Interval <= 0 {
		o.Interval = DefaultPollInterval
	}
	s, err := c.CreateServer(ctx, req)
	if err != nil {
		return nil, Classify(err)
	}
	deadline := time.Now().Add(o.Timeout)
	for {
		if s.Status == osapi.StatusError {
			return nil, deleteFailed(ctx, c, s)
		}
		if s.Status != osapi.StatusBuild || s.TaskState != osapi.TaskScheduling || !time.Now().Before(deadline) {
			return s, nil
		}
		t := time.NewTimer(min(o.Interval, time.Until(deadline)))
		select {
		case <-ctx.Done():
			t.Stop()
			return s, nil
		case <-t.C:
		}
		next, err := c.GetServer(ctx, s.ID)
		if err != nil {
			return s, nil
		}
		s = next
	}
}

// deleteFailed deletes a server that went to ERROR and returns the fault's
// classification, or Retryable when the server could not be deleted.
func deleteFailed(ctx context.Context, c osapi.Compute, s *osapi.Server) error {
	fault := ClassifyFault(s.Fault)
	if err := c.DeleteServer(ctx, s.ID); err != nil && !IsNotFound(err) {
		return vendors.NewError(vendors.Retryable, CodeErrorServerLeft,
			fmt.Errorf("server %s went to ERROR (%w) and deleting it failed: %w", s.ID, fault, err))
	}
	return fault
}

// FindByKey picks, among the servers of a name lookup, the one that carries the
// idempotency key and is a live attempt: not deleted, not being deleted and not
// in ERROR. Servers with the key in ERROR are returned as stale, for the
// caller to delete before it creates again.
func FindByKey(servers []osapi.Server, key string) (match *osapi.Server, stale []osapi.Server) {
	if key == "" {
		return nil, nil
	}
	for i := range servers {
		s := servers[i]
		if s.Metadata[TagKey(vendors.TagIdempotencyKey)] != key {
			continue
		}
		switch {
		case strings.EqualFold(s.TaskState, osapi.TaskDeleting) || ServerState(s) == vendors.StateTerminated:
		case s.Status == osapi.StatusError:
			stale = append(stale, s)
		case match == nil:
			match = &servers[i]
		}
	}
	return match, stale
}
