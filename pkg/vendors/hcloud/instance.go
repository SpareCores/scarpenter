/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package hcloud

import (
	"context"
	"fmt"
	"strconv"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// CreateInstance implements vendors.Driver: one POST /servers (plan 15,
// "CreateInstance"). The zone is the location; the resources of the launch
// come from Prepared (network, firewalls, placement group, volumes).
func (d *Driver) CreateInstance(ctx context.Context, req vendors.CreateRequest) (*vendors.Instance, error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	st, err := decodeSettings(req.Settings)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, CodeInvalidSettings, err)
	}
	imageID, err := strconv.ParseInt(req.Image.Ref, 10, 64)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, CodeInvalidSettings, fmt.Errorf(
			"the image reference %q is not a Hetzner image ID", req.Image.Ref))
	}
	location := req.Zone
	if location == "" {
		location = req.Region
	}
	name := req.Node.Name
	if name == "" {
		name = req.Tags[vendors.TagNodeClaim]
	}
	spec := LaunchSpec{
		Name: name, ServerType: req.InstanceType, Location: location, ImageID: imageID, UserData: req.UserData,
		Tags: req.Tags, ExtraTags: req.ExtraTags, IdempotencyKey: req.IdempotencyKey,
		PublicIPv4: boolOr(req.Settings.Networking.PublicIPv4, true),
		PublicIPv6: boolOr(req.Settings.Networking.PublicIPv6, false),
	}
	if req.Prepared != nil {
		for _, r := range req.Prepared.Resources {
			id, err := strconv.ParseInt(r.ID, 10, 64)
			if err != nil {
				return nil, vendors.NewError(vendors.Fatal, CodeInvalidSettings, fmt.Errorf(
					"the %s reference %q is not a Hetzner ID", r.Kind, r.ID))
			}
			switch r.Kind {
			case KindNetworks:
				spec.NetworkID = id
			case KindFirewalls:
				spec.FirewallIDs = append(spec.FirewallIDs, id)
			case KindPlacementGroups:
				spec.PlacementGroupID = id
			case KindVolumes:
				spec.VolumeIDs = append(spec.VolumeIDs, id)
			}
		}
	}
	if st.SSHKey != "" {
		if spec.SSHKeyID, err = d.sshKeyID(ctx, st.SSHKey); err != nil {
			return nil, err
		}
	}
	inst, err := d.p.Create(ctx, spec)
	d.warnRateLimit()
	return inst, err
}

// sshKeyID resolves the key name or ID once per process; the key must exist.
func (d *Driver) sshKeyID(ctx context.Context, idOrName string) (int64, error) {
	d.mu.Lock()
	id, ok := d.sshKeys[idOrName]
	d.mu.Unlock()
	if ok {
		return id, nil
	}
	id, err := d.p.ResolveSSHKey(ctx, idOrName)
	if err != nil {
		return 0, err
	}
	d.mu.Lock()
	d.sshKeys[idOrName] = id
	d.mu.Unlock()
	return id, nil
}

// GetInstance implements vendors.Driver. A server the API does not know is
// NotFound (Hetzner has no terminated state).
func (d *Driver) GetInstance(ctx context.Context, ref vendors.InstanceRef) (*vendors.Instance, error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	return d.p.Get(ctx, ref)
}

// FindInstance implements vendors.Driver, from the shared project-wide list.
func (d *Driver) FindInstance(
	ctx context.Context, region string, tags map[vendors.TagKey]string,
) (*vendors.Instance, error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	return d.p.Find(ctx, region, tags)
}

// ListInstances implements vendors.Driver, from the shared project-wide list.
func (d *Driver) ListInstances(
	ctx context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]*vendors.Instance, error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	out, err := d.p.List(ctx, regions, tags)
	d.warnRateLimit()
	return out, err
}

// DeleteInstance implements vendors.Driver: it refuses a server without this
// cluster's marker, and removes the Primary IPs the delete left behind.
func (d *Driver) DeleteInstance(ctx context.Context, ref vendors.InstanceRef) error {
	if err := d.ready(); err != nil {
		return err
	}
	err := d.p.Delete(ctx, ref)
	d.warnRateLimit()
	return err
}
