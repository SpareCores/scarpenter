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
	"time"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Codes of the errors the instance calls raise themselves.
const (
	codeUserDataTooLarge = "UserDataTooLarge"
	codePlanNotOffered   = "PlanNotOffered"
)

// inboundOf converts the NodeClass's inbound rules to the firewall's: one entry
// per rule and CIDR (the protocol any has no ports).
func inboundOf(in *common.Inbound) []Inbound {
	if in == nil {
		return nil
	}
	var out []Inbound
	for _, r := range in.Rules {
		proto := string(r.Protocol)
		if proto == "any" {
			proto = ""
		}
		var from, to int
		if r.FromPort != nil {
			from, to = int(*r.FromPort), int(*r.FromPort)
			if r.ToPort != nil {
				to = int(*r.ToPort)
			}
		}
		for _, c := range r.CIDRs {
			out = append(out, Inbound{CIDR: c, Protocol: proto, PortStart: from, PortEnd: to})
		}
	}
	return out
}

// prepared picks the resources of a launch out of what Prepare made ready.
type prepared struct {
	network, ruleset, group string
}

func preparedOf(p *vendors.Prepared) prepared {
	var out prepared
	if p == nil {
		return out
	}
	for _, r := range p.Resources {
		switch r.Kind {
		case KindNetworks:
			if out.network == "" {
				out.network = r.ID
			}
		case KindFirewalls:
			if out.ruleset == "" {
				out.ruleset = r.ID
			}
		case KindServerGroups:
			if out.group == "" {
				out.group = r.ID
			}
		}
	}
	return out
}

// launchSpec resolves a CreateRequest into the launch spec (the pure layer of
// launch.go builds the call) and the steps after the call.
func (d *Driver) launchSpec(ctx context.Context, req vendors.CreateRequest) (LaunchSpec, PostCreate, error) {
	plans, err := d.planTable(ctx)
	if err != nil {
		return LaunchSpec{}, PostCreate{}, err
	}
	plan, ok := plans[req.InstanceType]
	if !ok {
		return LaunchSpec{}, PostCreate{}, vendors.NewError(vendors.Unsupported, codePlanNotOffered, fmt.Errorf(
			"the plan %q is not in the account's plan list", req.InstanceType))
	}
	st, err := decodeSettings(req.Settings)
	if err != nil {
		return LaunchSpec{}, PostCreate{}, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	zone := req.Zone
	if zone == "" {
		zone = req.Region
	}
	root := rootOf(st)
	tmpl, err := d.templateFor(ctx, zone, req.Image.Ref, root.Tier, plan)
	if err != nil {
		return LaunchSpec{}, PostCreate{}, err
	}
	rootGB := root.SizeGB
	if rootGB == 0 {
		rootGB = max(plan.DefaultRootGB(), tmpl.SizeGB)
	}
	pr := preparedOf(req.Prepared)
	net := req.Settings.Networking
	labels, err := ServerLabels(req.Tags, req.ExtraTags, req.IdempotencyKey, d.clock.Now())
	if err != nil {
		return LaunchSpec{}, PostCreate{}, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	spec := LaunchSpec{
		Zone: zone, Plan: plan, Name: req.Node.Name, Template: tmpl,
		Root: Root{Tier: tmpl.Tier, SizeGB: rootGB, Encrypted: root.Encrypted},
		Data: dataOf(st),
		Networking: Networking{
			PublicIPv4: boolOr(net.PublicIPv4, true), PublicIPv6: boolOr(net.PublicIPv6, false), PrivateNetwork: pr.network,
		},
		ServerGroup: pr.group, UserData: req.UserData, Labels: labels,
	}
	if spec.Name == "" {
		spec.Name = req.Tags[vendors.TagNodeClaim]
	}
	rules, err := d.publicRules(ctx, req)
	if err != nil {
		return LaunchSpec{}, PostCreate{}, err
	}
	post := PostCreate{Rules: rules}
	if pr.network != "" {
		post.PrivateRuleset = pr.ruleset
	}
	return spec, post, nil
}

// CreateInstance implements vendors.Driver: one create call with the NodeClaim's
// UID as a label (UpCloud has no create token), looked up before every create and
// after a failure that may have lost the response. A replay of the key returns the
// original server whatever zone or plan it asks for. After the call, while the
// server is still cloning its storage, the Public and Utility firewall is written
// (and the SDN ruleset attached when there is a private interface); a server whose
// steps fail is deleted, so that the key stays unused.
func (d *Driver) CreateInstance(ctx context.Context, req vendors.CreateRequest) (*vendors.Instance, error) {
	if len(req.UserData) > MaxUserData {
		return nil, vendors.NewError(vendors.Fatal, codeUserDataTooLarge, fmt.Errorf(
			"the user-data is %d bytes; the budget is %d (a larger cluster CA or Ignition addition does not fit)",
			len(req.UserData), MaxUserData))
	}
	if found, err := FindByIdempotencyKey(ctx, d.api, req.IdempotencyKey); err != nil {
		return nil, err
	} else if found != nil {
		return d.replay(ctx, req, found)
	}
	spec, post, err := d.launchSpec(ctx, req)
	if err != nil {
		return nil, err
	}
	r, err := BuildCreateServer(spec)
	if err != nil {
		return nil, err
	}
	detail, err := CreateServer(ctx, d.api, r, StorageLabels(req.Tags)...)
	if err != nil {
		if vendors.IsClass(err, vendors.Retryable) {
			// A timeout or a lost response may have created the server.
			if found, ferr := FindByIdempotencyKey(ctx, d.api, req.IdempotencyKey); ferr == nil && found != nil {
				return d.replay(ctx, req, found)
			}
		}
		return nil, err
	}
	if err := post.Run(ctx, d.api, detail); err != nil {
		d.discard(ctx, detail.UUID)
		return nil, err
	}
	return ToInstance(detail), nil
}

// replay returns the server a replayed key created, rewriting its firewall when it
// is missing: the first attempt may have been interrupted between the create call
// and the firewall write, and a node must never run on the account's default rules.
func (d *Driver) replay(
	ctx context.Context, req vendors.CreateRequest, found *upcloud.ServerDetails,
) (*vendors.Instance, error) {
	rules, err := d.publicRules(ctx, req)
	if err != nil {
		return nil, err
	}
	cctx, cancel := CallContext(ctx, CallTimeout)
	have, err := d.api.GetFirewallRules(cctx, &request.GetFirewallRulesRequest{ServerUUID: found.UUID})
	cancel()
	if err != nil {
		return nil, Classify(err)
	}
	if have == nil || len(have.FirewallRules) != len(rules) {
		if err := WriteFirewall(ctx, d.api, found.UUID, rules); err != nil {
			return nil, err
		}
	}
	return ToInstance(found), nil
}

// publicRules are the Public & Utility firewall rules of a launch: the NodeClass's
// inbound rules, and Tailscale's UDP port only where the NodeClass uses the
// tailscale template (read from the NodeClass: the launch does not carry it).
func (d *Driver) publicRules(ctx context.Context, req vendors.CreateRequest) ([]upcloud.FirewallRule, error) {
	cfg, err := d.classResources(ctx, req.Tags[vendors.TagNodeClass])
	if err != nil {
		return nil, err
	}
	rules, err := PublicFirewallRules(FirewallOptions{
		Inbound: inboundOf(req.Settings.Networking.Inbound), Tailscale: cfg.tailscale,
	})
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	return rules, nil
}

// discard deletes a server whose create steps failed: it waits for the clone to
// finish (a server in maintenance cannot be stopped), then stops and deletes it.
// Failures are logged: the idempotency lookup deletes servers without storage, and
// the garbage collector and the sweeper find the rest by their labels.
func (d *Driver) discard(ctx context.Context, uuid string) {
	wctx, cancel := CallContext(ctx, CreateTimeout)
	defer cancel()
	_, _ = d.api.WaitForServerState(wctx, &request.WaitForServerStateRequest{
		UUID: uuid, DesiredState: upcloud.ServerStateStarted,
	})
	if err := Terminate(ctx, d.api, uuid); err != nil {
		d.log.Error(err, "deleting a server whose launch steps failed", "server", uuid)
	}
}

// byRef reads the server of a reference; NotFound for a reference that cannot
// exist and for a server that is gone or in another zone.
func (d *Driver) byRef(ctx context.Context, ref vendors.InstanceRef) (*upcloud.ServerDetails, error) {
	zone, uuid, ok := ParseRef(ref)
	if !ok {
		return nil, vendors.Errorf(vendors.NotFound, "the instance reference %q is not <zone>/<server uuid>", ref)
	}
	s, err := GetServer(ctx, d.api, uuid)
	if err != nil {
		return nil, err
	}
	if s.Zone != zone {
		return nil, vendors.Errorf(vendors.NotFound, "the server %s is not in %s", uuid, zone)
	}
	return s, nil
}

// GetInstance implements vendors.Driver. A deleted server does not exist at UpCloud
// (there is no terminated state): NotFound, which the contract allows.
func (d *Driver) GetInstance(ctx context.Context, ref vendors.InstanceRef) (*vendors.Instance, error) {
	s, err := d.byRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	return ToInstance(s), nil
}

// tagLabels are the server labels of logical tags.
func (d *Driver) tagLabels(tags map[vendors.TagKey]string) map[string]string {
	out := map[string]string{}
	for _, k := range vendors.TagKeys {
		if v, ok := tags[k]; ok {
			out[TagKey(k)] = v
		}
	}
	return out
}

// FindInstance implements vendors.Driver: the newest server in the zone with all
// the tags (stopped ones included: they are terminating, and found so that they
// can be deleted).
func (d *Driver) FindInstance(ctx context.Context, region string, tags map[vendors.TagKey]string,
) (*vendors.Instance, error) {
	labels := d.tagLabels(tags)
	if len(labels) == 0 {
		return nil, vendors.Errorf(vendors.NotFound, "no tags to find an instance by")
	}
	servers, err := ServersByLabels(ctx, d.api, labels)
	if err != nil {
		return nil, err
	}
	var best *upcloud.ServerDetails
	for i := range servers {
		s := &servers[i]
		if s.Zone != region {
			continue
		}
		if best == nil || createdOf(s).After(createdOf(best)) {
			best = s
		}
	}
	if best == nil {
		return nil, vendors.Errorf(vendors.NotFound, "no server in %s has the tags %v", region, tags)
	}
	return ToInstance(best), nil
}

func createdOf(s *upcloud.ServerDetails) time.Time {
	t, _ := Created(s.Labels)
	return t
}

// ListInstances implements vendors.Driver: SCarpenter's servers (the managed-by
// label) in the zones that carry all the tags, the stopped ones included (they
// are terminating and the garbage collector deletes them from this list). The
// list call has no labels in its answer, so each match costs a details call.
func (d *Driver) ListInstances(ctx context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]*vendors.Instance, error) {
	labels := d.tagLabels(tags)
	labels[TagKey(vendors.TagManagedBy)] = vendors.ManagedByValue
	servers, err := ServersByLabels(ctx, d.api, labels)
	if err != nil {
		return nil, fmt.Errorf("listing the servers: %w", err)
	}
	var out []*vendors.Instance
	for i := range servers {
		if len(regions) > 0 && !slices.Contains(regions, servers[i].Zone) {
			continue
		}
		out = append(out, ToInstance(&servers[i]))
	}
	return out, nil
}

// DeleteInstance implements vendors.Driver: stop, wait, delete with the storages;
// a server that does not exist is success. A server that does not carry this
// cluster's label is not ours (a wrong or tampered reference must not delete a
// foreign server, since the token is the account's) and is refused with a Fatal
// error.
func (d *Driver) DeleteInstance(ctx context.Context, ref vendors.InstanceRef) error {
	s, err := d.byRef(ctx, ref)
	if err != nil {
		if vendors.IsClass(err, vendors.NotFound) {
			return nil
		}
		return err
	}
	if tags := TagsOf(s.Labels); d.cfg.ClusterName == "" || tags[vendors.TagCluster] != d.cfg.ClusterName {
		return vendors.Errorf(vendors.Fatal, "the server %s does not carry the label of cluster %q and is not deleted",
			ref, d.cfg.ClusterName)
	}
	return Terminate(ctx, d.api, s.UUID)
}
