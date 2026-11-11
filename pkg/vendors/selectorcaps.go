/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vendors

import "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"

// Side says who evaluates a field of the selector (resource-selectors.md,
// "Matching"). The shared matcher re-applies every field it has the data for
// to what the vendor returned, so a vendor may narrow to a superset.
type Side string

// Sides.
const (
	// SideServer: the vendor evaluates the field exactly.
	SideServer Side = "server"
	// SideServerClient: the vendor narrows to a superset and the client
	// finishes.
	SideServerClient Side = "server+client"
	// SideClient: the resources are listed and matched locally.
	SideClient Side = "client"
)

// TagForm is how a kind carries tags.
type TagForm string

// Tag forms.
const (
	// TagsNone: the kind has no tags or labels; `tags` is rejected.
	TagsNone TagForm = ""
	// TagsMap: key and value (AWS tags).
	TagsMap TagForm = "tags"
	// TagsLabels: key and value labels (hcloud, GCP, UpCloud).
	TagsLabels TagForm = "labels"
	// TagsStrings: plain strings, the pair k: v is the string "k=v"
	// (OpenStack).
	TagsStrings TagForm = "strings"
)

// The portable attributes of `match`, by their field names.
const (
	MatchArchitecture      = "architecture"
	MatchOwner             = "owner"
	MatchState             = "state"
	MatchIncludeDeprecated = "includeDeprecated"
	MatchCreatedAfter      = "createdAfter"
	MatchCreatedBefore     = "createdBefore"
	MatchDescription       = "description"
	MatchZone              = "zone"
	MatchNetwork           = "network"
	MatchCIDR              = "cidr"
	MatchWithin            = "within"
)

// MatchAttributes lists the attributes of `match` in a fixed order.
var MatchAttributes = []string{
	MatchArchitecture, MatchOwner, MatchState, MatchIncludeDeprecated, MatchCreatedAfter, MatchCreatedBefore,
	MatchDescription, MatchZone, MatchNetwork, MatchCIDR, MatchWithin,
}

// FieldCap is one field a driver supports: the vendor's native field it maps
// to and who evaluates it.
type FieldCap struct {
	// Field is the vendor's field or filter name (documentation and tests).
	Field string
	Side  Side
}

// FilterCap is a name accepted under `filters`: an exact name, or a pattern
// that ends in `*` (tag:*, labels.*, property:*), with the side and, for a
// vendor that may silently ignore a parameter, the response field that
// re-applies it.
type FilterCap struct {
	Name          string
	Side          Side
	ResponseField string
}

// SelectorCaps is what a driver supports of the selector for one kind
// (ADR-023). ValidateNodeClass rejects everything outside it; the conformance
// suite and docs/reference/resource-selectors.md are kept in step with it.
type SelectorCaps struct {
	// Zonal says that the kind lives in a zone: `{zone}` is expanded per zone
	// and `match.zone` is accepted.
	Zonal bool
	// Global says that the kind is global at the vendor (hcloud firewalls, Vultr
	// firewall groups, GCP images): its listing does not depend on the region.
	// The core lists it once per check and reuses the answer for every region
	// (Resolve is called with the first region in scope).
	Global bool
	// HasCreated says that the kind has a creation time: `created` as orderBy
	// and createdAfter and createdBefore are accepted.
	HasCreated bool
	// Tags is the tag form; TagsNone rejects `tags`.
	Tags TagForm
	// Name is where `name` is evaluated and the field it is matched against; nil
	// rejects `name`.
	Name *FieldCap
	// Match maps each supported `match` attribute to its field and side;
	// absent is unsupported (rejected).
	Match map[string]FieldCap
	// Filters are the accepted `filters` names.
	Filters []FilterCap
	// Expression is the language of `expression` (gcp-filter, resource-graph,
	// label-selector); "" rejects it.
	Expression string
	// DefaultSelect is the select used when the NodeClass gives none.
	DefaultSelect common.Select
}
