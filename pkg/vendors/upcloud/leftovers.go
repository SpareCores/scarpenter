/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import (
	"context"
	"errors"
	"fmt"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// CollectLeftovers implements vendors.LeftoverCollector: the storages of this
// cluster in the zone that a node's delete left behind, because the storage
// delete failed after the server was gone. A storage qualifies when it carries the
// labels of a node's storage (StorageLabels: this cluster, the managed-by marker
// and a NodeClaim), no scarpenter-use, is online and attached to no server, and its
// NodeClaim does not exist. Nothing else is touched, whatever it is attached to or
// called: a volume of someone else carries none of these labels.
func (d *Driver) CollectLeftovers(
	ctx context.Context, region string, nodeClaimExists func(name string) bool,
) (int, error) {
	if d.cfg.ClusterName == "" {
		return 0, nil
	}
	lctx, cancel := CallContext(ctx, CallTimeout)
	list, err := d.api.GetStorages(lctx, &request.GetStoragesRequest{
		Access: upcloud.StorageAccessPrivate,
		Filters: []request.QueryFilter{
			request.FilterLabel{Label: upcloud.Label{Key: TagKey(vendors.TagCluster), Value: d.cfg.ClusterName}},
		},
	})
	cancel()
	if err != nil {
		return 0, Classify(err)
	}
	deleted := 0
	var errs []error
	for _, st := range list.Storages {
		if st.Zone != region || st.Type == upcloud.StorageTypeTemplate || st.Type == upcloud.StorageTypeCDROM ||
			st.Type == upcloud.StorageTypeBackup {
			continue
		}
		claim := TagsOf(st.Labels)[vendors.TagNodeClaim]
		if !IsNodeStorage(st.Labels, d.cfg.ClusterName, claim) {
			continue
		}
		// Read the storage again: it may have been attached or deleted since the list.
		gctx, cancel := CallContext(ctx, CallTimeout)
		cur, err := d.api.GetStorageDetails(gctx, &request.GetStorageDetailsRequest{UUID: st.UUID})
		cancel()
		if err != nil {
			if !isGone(Classify(err)) {
				errs = append(errs, fmt.Errorf("reading the storage %s: %w", st.UUID, Classify(err)))
			}
			continue
		}
		if len(cur.ServerUUIDs) > 0 || cur.State != upcloud.StorageStateOnline ||
			!IsNodeStorage(cur.Labels, d.cfg.ClusterName, claim) {
			continue
		}
		// Asked after the listing, for each storage: a launch in flight has its NodeClaim.
		if nodeClaimExists(claim) {
			continue
		}
		gone, err := DeleteStorage(ctx, d.api, st.UUID)
		if err != nil {
			errs = append(errs, fmt.Errorf("deleting the storage %s: %w", st.UUID, err))
			continue
		}
		if gone { // not one that was attached or gone in the meantime
			deleted++
		}
	}
	if len(errs) > 0 {
		return deleted, Classify(errors.Join(errs...))
	}
	return deleted, nil
}

var _ vendors.LeftoverCollector = (*Driver)(nil)
