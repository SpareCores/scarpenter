/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"context"
	"strings"
	"sync"
	"time"

	"k8s.io/utils/clock"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Per-call deadlines (plan 21 section 3): 45 s, 90 s for RunInstances (as AWS).
const (
	callTimeout = 45 * time.Second
	runTimeout  = 90 * time.Second
)

// Retry bounds of the driver's own backoff on throttling and transient server
// errors: the SDK has per-call retry settings but no adaptive mode.
const (
	maxAttempts    = 4
	initialBackoff = 500 * time.Millisecond
	maxBackoff     = 8 * time.Second
)

// Cloud is the client layer of the driver: ECS and VPC clients per region
// (created lazily, once), a bounded retry with a per-call deadline, and the
// classification of every error into the contract's classes. The instance
// operations, the quota reader and the (later) driver are methods or users of
// it.
type Cloud struct {
	cluster string
	factory ClientFactory
	clock   clock.Clock
	// sleep waits between attempts; tests replace it.
	sleep func(ctx context.Context, d time.Duration) error

	// Types is the instance type table the quota reader and the launch use.
	Types *TypeTable

	mu      sync.Mutex
	clients map[string]Clients
}

// NewCloud returns the client layer of a cluster. A nil clock is the real one.
func NewCloud(cluster string, factory ClientFactory, c clock.Clock) *Cloud {
	if c == nil {
		c = clock.RealClock{}
	}
	return &Cloud{
		cluster: cluster, factory: factory, clock: c, sleep: sleepCtx,
		Types: NewTypeTable(), clients: map[string]Clients{},
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Clients returns the clients of the region, creating them on first use.
func (c *Cloud) Clients(ctx context.Context, region string) (Clients, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cl, ok := c.clients[region]; ok {
		return cl, nil
	}
	cl, err := c.factory(ctx, region)
	if err != nil {
		return Clients{}, vendors.NewError(vendors.Fatal, "", err)
	}
	c.clients[region] = cl
	return cl, nil
}

// retryable reports whether the error of a call is one the driver retries on
// the spot: throttling, ServiceUnavailable and InternalError (transport errors
// and deadlines are not retried here: the caller's loop owns them).
func retryable(err error) bool {
	if vendors.ClassOf(err) != vendors.Retryable {
		return false
	}
	switch code := vendors.CodeOf(err); code {
	case "ServiceUnavailable", "InternalError", codeServerError:
		return true
	default:
		return strings.HasPrefix(code, "Throttling")
	}
}

// call runs fn with the deadline and the bounded backoff, and returns its
// error classified. fn receives the context of the attempt.
func call[T any](
	ctx context.Context, c *Cloud, timeout time.Duration, ec errContext, fn func(ctx context.Context) (T, error),
) (T, error) {
	var zero T
	backoff := initialBackoff
	for attempt := 1; ; attempt++ {
		actx, cancel := context.WithTimeout(ctx, timeout)
		out, err := fn(actx)
		cancel()
		if err == nil {
			return out, nil
		}
		err = classify(err, ec)
		// The caller's own cancellation or deadline ends the loop.
		if ctx.Err() != nil || !retryable(err) || attempt >= maxAttempts {
			return zero, err
		}
		if serr := c.sleep(ctx, backoff); serr != nil {
			return zero, classify(serr, ec)
		}
		backoff = min(backoff*2, maxBackoff)
	}
}
