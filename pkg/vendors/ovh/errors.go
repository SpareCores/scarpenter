/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
)

// Error codes the driver itself raises or assigns to the texts it matches.
const (
	CodeNoValidHost      = "NoValidHost"
	CodeMaxRetries       = "MaxRetriesExceeded"
	CodeOverQuota        = "OverQuota"
	CodeFlavorUnusable   = "FlavorUnusable"
	CodeIPExhausted      = "IPAddressExhausted"
	CodeNoEndpoint       = "NoEndpoint"
	CodeRegionDenied     = "RegionDenied"
	CodeThrottled        = "Throttled"
	CodeServerError      = "ServerError"
	CodeConflict         = "Conflict"
	CodeNotFound         = "NotFound"
	CodeTransport        = "TransportError"
	CodeTimeout          = "CallTimeout"
	CodeCanceled         = "CallCanceled"
	CodeServerFault      = "ServerFault"
	CodeImageNotActive   = "ImageNotActive"
	CodeImageMissing     = "ImageNotFound"
	CodeUserDataTooLarge = "UserDataTooLarge"
	CodeInvalidSettings  = "InvalidSettings"
	CodeBadRequest       = "BadRequest"
	CodeUnauthorized     = "Unauthorized"
	CodeForbidden        = "Forbidden"
	CodeAmbiguousGroup   = "AmbiguousSecurityGroup"
)

// codeHints are prefixed to the message of a classified error where the
// vendor's text alone does not say what the operator must change.
var codeHints = map[string]string{
	CodeAmbiguousGroup: "the security group name matches more than one group in the region " +
		"(Nova attaches groups by name): rename one or hand over a group whose name is unique",
	CodeUnauthorized: "the OpenStack credentials were rejected (expired, revoked or wrong)",
}

// Service names of osapi.APIError.
const (
	svcNova     = "nova"
	svcNeutron  = "neutron"
	svcKeystone = "keystone"
)

// textRule is one message-based rule: the class applies when the HTTP status is
// one of Status and the message contains Contains (case-insensitive), and, when
// Service is set, the call was made to that service. OpenStack has no stable
// error codes for these failures, so each text is documented in
// docs/reference/ovh-driver.md, pinned by the table test, and marked for the
// spike S7 to confirm on the real service. A text decides nothing without its
// status, and a message that matches nothing is Fatal, never
// InsufficientCapacity.
type textRule struct {
	Status   []int
	Service  string
	Contains string
	Class    vendors.Class
	Code     string
}

// textRules are matched in order; the first match wins.
var textRules = []textRule{
	// Stock: the scheduler found no host. Nova answers 202 and the failure
	// shows later as the fault of a server in ERROR (fault code 500).
	{[]int{500}, svcNova, "No valid host was found", vendors.InsufficientCapacity, CodeNoValidHost},
	// Nova's text when the scheduler's retries are used up (not in the plan:
	// the same cause after a failed build on a chosen host).
	{[]int{500}, svcNova, "Exceeded maximum number of retries", vendors.InsufficientCapacity, CodeMaxRetries},
	// Per-project, per-region limits of Nova (instances, cores, ram,
	// server_groups): Nova's OverQuota, HTTP 403. A metadata items quota is a bug
	// of ours and falls through to Fatal.
	{[]int{403}, svcNova, "Quota exceeded for instances", vendors.QuotaExceeded, CodeOverQuota},
	{[]int{403}, svcNova, "Quota exceeded for cores", vendors.QuotaExceeded, CodeOverQuota},
	{[]int{403}, svcNova, "Quota exceeded for ram", vendors.QuotaExceeded, CodeOverQuota},
	{[]int{403}, svcNova, "Quota exceeded for server_groups", vendors.QuotaExceeded, CodeOverQuota},
	{[]int{403}, svcNova, "Quota exceeded, too many server groups", vendors.QuotaExceeded, CodeOverQuota},
	// Nova checks Neutron's port quota before it accepts the create
	// (PortLimitExceeded, HTTP 403): every node takes one Ext-Net port.
	{[]int{403}, svcNova, "Maximum number of ports exceeded", vendors.QuotaExceeded, CodeOverQuota},
	// Neutron's OverQuota for ports and security groups (HTTP 409; the type is
	// prefixed to the message by the client layer).
	{[]int{409}, svcNeutron, "OverQuota", vendors.QuotaExceeded, CodeOverQuota},
	// The type cannot run here: the flavor does not exist in the region or its
	// disk or memory is too small for the image.
	{[]int{400}, svcNova, "Invalid flavorRef provided", vendors.Unsupported, CodeFlavorUnusable},
	{[]int{400}, svcNova, "Flavor's disk is too small", vendors.Unsupported, CodeFlavorUnusable},
	{[]int{400}, svcNova, "Flavor's memory is too small", vendors.Unsupported, CodeFlavorUnusable},
	// Ext-Net is out of addresses (unverified on OVH): the zone cannot host
	// another node.
	{[]int{409, 500}, "", "No more IP addresses available", vendors.ZoneUnusable, CodeIPExhausted},
	// Nova attaches security groups by name; a name more than one group in the
	// project has is NoUniqueMatch (HTTP 409). A retry fails the same way: it is
	// a configuration error, not a conflict that passes.
	{[]int{409}, svcNova, "Multiple security groups found", vendors.Fatal, CodeAmbiguousGroup},
	// The image of the call is gone or not usable: the hand-off is wrong, no
	// other candidate helps.
	{[]int{400}, svcNova, "Invalid imageRef provided", vendors.Fatal, CodeImageMissing},
	// Nova's text for a well-formed image ID that Glance does not have.
	{[]int{400}, svcNova, "Can not find requested image", vendors.Fatal, CodeImageMissing},
	{[]int{400}, svcNova, "is not active", vendors.Fatal, CodeImageNotActive},
}

// Classify wraps an error of an OpenStack call into the driver's error classes.
// Errors that are already classified pass through unchanged. The table is
// textRules plus the status rules below:
//
//	no endpoint in the catalog, Keystone 401/403   RegionUnavailable
//	404                                            NotFound
//	429, 5xx, 409 (not a quota), transport errors  Retryable
//	everything else                                Fatal
func Classify(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*vendors.Error](err); ok {
		return err
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return vendors.NewError(vendors.Retryable, CodeTimeout, err)
	case errors.Is(err, context.Canceled):
		return vendors.NewError(vendors.Retryable, CodeCanceled, err)
	case errors.Is(err, osapi.ErrNoEndpoint):
		return vendors.NewError(vendors.RegionUnavailable, CodeNoEndpoint, err)
	}
	if ae, ok := errors.AsType[*osapi.APIError](err); ok {
		return classifyStatus(ae.Service, ae.Status, ae.Message, err)
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return vendors.NewError(vendors.Retryable, CodeTransport, err)
	}
	return vendors.NewError(vendors.Fatal, "", err)
}

func classifyStatus(service string, status int, msg string, err error) error {
	lower := strings.ToLower(msg)
	for _, r := range textRules {
		if (r.Service == "" || r.Service == service) && containsInt(r.Status, status) &&
			strings.Contains(lower, strings.ToLower(r.Contains)) {
			return newError(r.Class, r.Code, err)
		}
	}
	switch {
	case service == svcKeystone && status == 401:
		// The credentials themselves: no region helps (the contract keeps
		// RegionUnavailable for a region the credentials cannot read).
		return newError(vendors.Fatal, CodeUnauthorized, err)
	case service == svcKeystone && status == 403:
		return vendors.NewError(vendors.RegionUnavailable, CodeRegionDenied, err)
	case status == 404:
		return vendors.NewError(vendors.NotFound, CodeNotFound, err)
	case status == 429:
		return vendors.NewError(vendors.Retryable, CodeThrottled, err)
	case status >= 500:
		return vendors.NewError(vendors.Retryable, CodeServerError, err)
	case status == 409:
		return vendors.NewError(vendors.Retryable, CodeConflict, err)
	case status == 401:
		return newError(vendors.Fatal, CodeUnauthorized, err)
	case status == 403:
		return vendors.NewError(vendors.Fatal, CodeForbidden, err)
	case status == 400:
		return vendors.NewError(vendors.Fatal, CodeBadRequest, err)
	}
	return vendors.NewError(vendors.Fatal, "", err)
}

func newError(class vendors.Class, code string, err error) error {
	if h, ok := codeHints[code]; ok {
		err = fmt.Errorf("%s: %w", h, err)
	}
	return vendors.NewError(class, code, err)
}

// ClassifyCatalog classifies an error of reading the catalog's regions
// (Provider.Regions), which is not about one region: a Keystone 401 or 403
// there is a credentials problem (Fatal), never RegionUnavailable, which the
// contract allows only from Zones for a region the credentials cannot read.
func ClassifyCatalog(err error) error {
	c := Classify(err)
	if vendors.IsClass(c, vendors.RegionUnavailable) {
		return vendors.NewError(vendors.Fatal, CodeForbidden,
			fmt.Errorf("the credentials may not read the service catalog: %w", err))
	}
	return c
}

// CatalogRegions reads the compute regions of the Keystone catalog and joins
// them with the region table (RegionMap.Join). TODO(plan 14): the driver's
// Regions is this plus the NodeClass allow-list.
func CatalogRegions(
	ctx context.Context, p osapi.Provider, m *RegionMap,
) (joined []vendors.Region, unknown []string, err error) {
	names, err := p.Regions(ctx)
	if err != nil {
		return nil, nil, ClassifyCatalog(err)
	}
	joined, unknown = m.Join(names)
	return joined, unknown, nil
}

func containsInt(s []int, v int) bool {
	return slices.Contains(s, v)
}

// ClassifyFault classifies the fault of a server that went to ERROR: the status
// is the fault's code (Nova reports 500 for scheduling failures). An unknown
// fault is Fatal, never capacity.
func ClassifyFault(f *osapi.Fault) error {
	if f == nil {
		return vendors.NewError(vendors.Fatal, CodeServerFault, errors.New("server in ERROR without a fault"))
	}
	err := &osapi.APIError{Service: svcNova, Status: f.Code, Message: f.Message}
	if c := classifyStatus(svcNova, f.Code, f.Message, err); vendors.ClassOf(c) != vendors.Retryable {
		return c
	}
	// A 5xx fault with a text we do not know is not retried blindly.
	return vendors.NewError(vendors.Fatal, CodeServerFault, err)
}

// IsNotFound reports whether the error is NotFound after classification.
func IsNotFound(err error) bool { return vendors.IsClass(Classify(err), vendors.NotFound) }
