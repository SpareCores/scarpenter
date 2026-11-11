/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"strings"
	"time"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// The resource kinds of the driver. images is a kind of the selector only
// (SCarpenter never creates images, ADR-021); the other two are managed.
const (
	KindImages                                 = selector.ImagesKind
	KindSecurityGroups    vendors.ResourceKind = "securityGroups"
	KindRunSecurityGroups vendors.ResourceKind = "runSecurityGroups"
	KindServerGroups      vendors.ResourceKind = "serverGroups"
)

// Glance image statuses, as the selector's portable states.
const (
	stateAvailable = "available"
	statePending   = "pending"
	stateFailed    = "failed"

	filterVisibility = "visibility"
	filterProperty   = "property:*"
	propertyPrefix   = "property:"
	propertyArch     = "architecture"
	fieldName        = "name"
)

// glanceState maps a Glance status to the portable state: active is available;
// queued, saving, uploading and importing are pending; everything else (killed,
// deactivated, deleted) is failed.
func glanceState(status string) string {
	switch status {
	case osapi.ImageActive:
		return stateAvailable
	case "queued", "saving", "uploading", "importing":
		return statePending
	}
	return stateFailed
}

// archProperty is the Glance property value of an architecture.
var archProperty = map[string]string{archAMD64: "x86_64", archARM64: "aarch64"}

// Selectors implements vendors.ManagedResources: what the `use` selector
// supports of each kind (docs/reference/resource-selectors.md, section ovh).
// Neutron silently ignores filters it does not know and the plan's spike S5 has
// not shown the tag extension on OVH, so the security groups and server groups
// offer no tags; nothing the driver cannot re-apply to a response is offered.
func (d *Driver) Selectors() map[vendors.ResourceKind]vendors.SelectorCaps {
	return map[vendors.ResourceKind]vendors.SelectorCaps{
		KindImages: {
			HasCreated: true,
			Tags:       vendors.TagsStrings,
			Name:       &vendors.FieldCap{Field: fieldName, Side: vendors.SideServerClient},
			Match: map[string]vendors.FieldCap{
				vendors.MatchArchitecture:  {Field: "property architecture", Side: vendors.SideClient},
				vendors.MatchOwner:         {Field: "owner", Side: vendors.SideServer},
				vendors.MatchState:         {Field: "status", Side: vendors.SideServerClient},
				vendors.MatchCreatedAfter:  {Field: "created_at", Side: vendors.SideClient},
				vendors.MatchCreatedBefore: {Field: "created_at", Side: vendors.SideClient},
			},
			Filters: []vendors.FilterCap{
				{Name: filterVisibility, Side: vendors.SideServerClient, ResponseField: "visibility"},
				{Name: filterProperty, Side: vendors.SideClient, ResponseField: "properties"},
			},
			DefaultSelect: common.SelectExactlyOne,
		},
		KindSecurityGroups: {
			Name: &vendors.FieldCap{Field: fieldName, Side: vendors.SideClient},
			Match: map[string]vendors.FieldCap{
				vendors.MatchDescription: {Field: "description", Side: vendors.SideClient},
			},
			DefaultSelect: common.SelectAll,
		},
		KindServerGroups: {
			Name:          &vendors.FieldCap{Field: fieldName, Side: vendors.SideClient},
			DefaultSelect: common.SelectExactlyOne,
		},
	}
}

// imageFields is the selector's view of a Glance image.
func imageFields(im osapi.Image) selector.Fields {
	attrs := map[string][]string{vendors.MatchState: {glanceState(im.Status)}, vendors.MatchOwner: {im.Owner}}
	attrs[vendors.MatchArchitecture] = []string{""} // an image that says nothing is never matched by architecture
	for arch, v := range archProperty {
		if strings.EqualFold(im.Properties[propertyArch], v) {
			attrs[vendors.MatchArchitecture] = []string{arch}
		}
	}
	return selector.Fields{
		ID: im.ID, Name: im.Name, TagStrings: im.Tags, Created: im.Created.UTC().Truncate(time.Second), Attrs: attrs,
	}
}

func groupFields(g osapi.SecurityGroup) selector.Fields {
	return selector.Fields{ID: g.ID, Name: g.Name, Attrs: map[string][]string{vendors.MatchDescription: {g.Description}}}
}

func serverGroupFields(g osapi.ServerGroup) selector.Fields {
	return selector.Fields{ID: g.ID, Name: g.Name}
}
