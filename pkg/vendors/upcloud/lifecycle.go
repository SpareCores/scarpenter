/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import (
	"context"
	"fmt"
	"slices"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/api"
)

// CreateServer runs the create call with its deadline and classifies the error.
// storageLabels (StorageLabels) go on every storage in the same call, so no
// storage of a node exists unlabelled. The server comes back in the maintenance
// state while its storage is cloned (and the storages cannot be modified then).
func CreateServer(
	ctx context.Context, a api.API, r *request.CreateServerRequest, storageLabels ...upcloud.Label,
) (*upcloud.ServerDetails, error) {
	ctx, cancel := CallContext(ctx, CreateTimeout)
	defer cancel()
	d, err := a.CreateServer(ctx, r, storageLabels...)
	return d, Classify(err)
}

// GetServer returns a server's details, NotFound (classified) when it is gone.
func GetServer(ctx context.Context, a api.API, uuid string) (*upcloud.ServerDetails, error) {
	ctx, cancel := CallContext(ctx, CallTimeout)
	defer cancel()
	d, err := a.GetServerDetails(ctx, &request.GetServerDetailsRequest{UUID: uuid})
	return d, Classify(err)
}

// ServersByLabels lists the servers that carry all the labels, across zones, in
// one call, and reads each match's details: the list answer has no labels, and
// the caller needs them. A server deleted between the two calls is skipped.
func ServersByLabels(ctx context.Context, a api.API, labels map[string]string) ([]upcloud.ServerDetails, error) {
	filters := make([]request.QueryFilter, 0, len(labels))
	for _, k := range sortedKeys(labels) {
		filters = append(filters, request.FilterLabel{Label: upcloud.Label{Key: k, Value: labels[k]}})
	}
	lctx, cancel := CallContext(ctx, CallTimeout)
	defer cancel()
	list, err := a.GetServersWithFilters(lctx, &request.GetServersWithFiltersRequest{Filters: filters})
	if err != nil {
		return nil, Classify(err)
	}
	var out []upcloud.ServerDetails
	for _, s := range list.Servers {
		d, err := GetServer(ctx, a, s.UUID)
		if err != nil {
			if vendors.IsClass(err, vendors.NotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, *d)
	}
	return out, nil
}

// FindByIdempotencyKey is the lookup of the idempotent create (UpCloud has no
// token): the server labelled scarpenter-idempotency-key=<key>, whatever its zone
// or plan, nil when there is none. Partial servers, those the label lookup finds
// with no storage (a create that failed half way), are deleted here, "by the next
// call" of plan 19; a server in error is not complete either. When a replay left
// duplicates, the first complete one by UUID wins.
func FindByIdempotencyKey(ctx context.Context, a api.API, key string) (*upcloud.ServerDetails, error) {
	if key == "" {
		return nil, nil
	}
	servers, err := ServersByLabels(ctx, a, map[string]string{TagKey(vendors.TagIdempotencyKey): key})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(servers, func(x, y upcloud.ServerDetails) int { return compare(x.UUID, y.UUID) })
	var found *upcloud.ServerDetails
	for i := range servers {
		s := &servers[i]
		if len(s.StorageDevices) == 0 {
			if err := Terminate(ctx, a, s.UUID); err != nil && !vendors.IsClass(err, vendors.Retryable) {
				return nil, err
			}
			continue
		}
		if found == nil {
			found = s
		}
	}
	return found, nil
}

func compare(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Terminate deletes a server: UpCloud deletes only a stopped one, so it hard
// stops it, waits for stopped, then deletes it with its storages and backups. A
// gone server (404) and one already stopped are success of their step; a server
// in maintenance (cloning, or a stop in progress) is Retryable. Every storage on
// a node server belongs to it, as the create call makes them, so none outlives
// it unless the call fails between the steps (the sweeper and the orphan pass
// cover that).
func Terminate(ctx context.Context, a api.API, uuid string) error {
	d, err := GetServer(ctx, a, uuid)
	if err != nil {
		if isGone(err) {
			return nil
		}
		return err
	}
	switch d.State {
	case upcloud.ServerStateMaintenance:
		return vendors.Errorf(vendors.Retryable, "server %s is in maintenance: cloning or a stop in progress", uuid)
	case upcloud.ServerStateStopped:
	default:
		if err := stop(ctx, a, uuid); err != nil {
			return err
		}
	}
	dctx, cancel := CallContext(ctx, CallTimeout)
	defer cancel()
	err = a.DeleteServerAndStorages(dctx, &request.DeleteServerAndStoragesRequest{
		UUID: uuid, Backups: request.DeleteStorageBackupsModeDelete,
	})
	if err != nil && !isGone(Classify(err)) {
		return classifyDelete(err)
	}
	return nil
}

func stop(ctx context.Context, a api.API, uuid string) error {
	sctx, cancel := CallContext(ctx, CallTimeout)
	_, err := a.StopServer(sctx, &request.StopServerRequest{UUID: uuid, StopType: request.ServerStopTypeHard})
	cancel()
	if err != nil {
		if isGone(Classify(err)) {
			return nil
		}
		// SERVER_STATE_ILLEGAL: a stop is already running, or the server just stopped.
		return Classify(err)
	}
	wctx, cancel := CallContext(ctx, StopTimeout)
	defer cancel()
	_, err = a.WaitForServerState(wctx, &request.WaitForServerStateRequest{
		UUID: uuid, DesiredState: upcloud.ServerStateStopped,
	})
	if err != nil && !isGone(Classify(err)) {
		return Classify(err)
	}
	return nil
}

// StorageLabels are the labels of a node's storages: the cluster, the NodeClaim
// and the managed-by marker, which the sweeper and the orphan pass find them by.
// They are set in the create call (CreateServer).
func StorageLabels(tags map[vendors.TagKey]string) []upcloud.Label {
	var out []upcloud.Label
	for _, k := range []vendors.TagKey{vendors.TagCluster, vendors.TagNodeClaim, vendors.TagManagedBy} {
		if v := tags[k]; v != "" {
			out = append(out, upcloud.Label{Key: TagKey(k), Value: v})
		}
	}
	return out
}

// PostCreate is what follows the create call, while the server is still in
// maintenance: write the Public & Utility firewall, and, with a private
// interface and a ruleset, attach the ruleset and switch the private firewall
// on. The server group and the storage labels are part of the create call.
type PostCreate struct {
	Rules []upcloud.FirewallRule
	// PrivateRuleset is the SDN ruleset UUID to attach to the private interface.
	PrivateRuleset string
}

// Run applies the steps in order; the first failure is returned classified, and
// the caller deletes the server (the idempotency lookup then finds nothing).
func (p PostCreate) Run(ctx context.Context, a api.API, d *upcloud.ServerDetails) error {
	if err := WriteFirewall(ctx, a, d.UUID, p.Rules); err != nil {
		return fmt.Errorf("writing the firewall: %w", err)
	}
	if p.PrivateRuleset == "" {
		return nil
	}
	actx, cancel := CallContext(ctx, CallTimeout)
	defer cancel()
	if err := a.AttachPrivateFirewallRuleset(actx, d.UUID, p.PrivateRuleset); err != nil {
		return fmt.Errorf("attaching the ruleset: %w", Classify(err))
	}
	if _, err := a.ModifyServer(actx, &request.ModifyServerRequest{UUID: d.UUID, FirewallPrivate: "on"}); err != nil {
		return fmt.Errorf("switching the private firewall on: %w", Classify(err))
	}
	return nil
}
