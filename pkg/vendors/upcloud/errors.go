/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import (
	"context"
	"errors"
	"net"
	"net/http"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/client"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Error codes the driver raises itself (UpCloud's are the API's).
const (
	CodeTimeout   = "CallTimeout"
	CodeCanceled  = "CallCanceled"
	CodeTransport = "TransportError"
	CodeHTTP5xx   = "ServerError"
	CodeThrottled = "Throttled"
)

// UpCloud error codes by name, for the codes the SDK has no constant for.
// Codes the plan lists with a name that is unverified are marked.
const (
	codeStrictAAUnavailable = "SERVER_RESOURCES_UNAVAILABLE_FOR_STRICT_AA"
	codeNetworkLimit        = "NETWORK_LIMIT_REACHED"
	codeFloatingIPLimit     = "DETACHED_FLOATING_IP_LIMIT_REACHED"
	codeLabelLimit          = "LABEL_LIMIT_REACHED"
	// The per-tier storage limits: the SDK has MAXIOPS_STORAGE_LIMIT_REACHED only;
	// the API reference (storage chapter) lists the standard and HDD ones too.
	codeStandardStorageLimit = "STANDARD_STORAGE_LIMIT_REACHED"
	codeHDDStorageLimit      = "HDD_STORAGE_LIMIT_REACHED"
	// The ruleset codes are unverified (the ruleset API is not in the v1.3
	// reference); unknown codes on deletes fall back by status (classifyDelete).
	codeRulesetNotFound = "FIREWALL_RULESET_NOT_FOUND"
	codeRulesetInUse    = "FIREWALL_RULESET_IN_USE"
)

// quotaCodes are the limit codes: UpCloud's limits are the account's, so a
// reached limit gates every zone (plan 14, A5).
var quotaCodes = []string{
	upcloud.ErrCodeServerCoresLimitReached,
	upcloud.ErrCodeServerMemoryLimitReached,
	upcloud.ErrCodeMaxiOpsStorageLimitReached,
	codeStandardStorageLimit,
	codeHDDStorageLimit,
	upcloud.ErrCodeServerIPLimitReached,
	codeNetworkLimit,
	codeFloatingIPLimit,
}

// errorClasses is the single table from the API's error codes to the contract's
// classes (plan 19, "Error mapping"); Classify reads it and a test covers every
// entry. The classification is by code, never by message text. Unlisted codes
// are Fatal, LABEL_LIMIT_REACHED among them (our bug), as are
// INSUFFICIENT_CREDITS (billing, not capacity: nothing should retry into a drained
// account) and ZONE_MISMATCH (a template or network in another zone: our bug).
var errorClasses = func() map[string]vendors.Class {
	m := map[string]vendors.Class{
		// Stock: no host has room right now. UpCloud has no stock API, so this
		// is the reactive signal.
		upcloud.ErrCodeServerResourcesUnavailable:    vendors.InsufficientCapacity,
		upcloud.ErrCodeStorageResourcesUnavailable:   vendors.InsufficientCapacity,
		codeStrictAAUnavailable:                      vendors.InsufficientCapacity,
		upcloud.ErrCodeIpAddressResourcesUnavailable: vendors.InsufficientCapacity,
		// The plan or tier does not exist in this combination.
		upcloud.ErrCodeCoreMemoryUnsupported:   vendors.Unsupported,
		upcloud.ErrCodePlanCoreNumberIllegal:   vendors.Unsupported,
		upcloud.ErrCodePlanMemoryAmountIllegal: vendors.Unsupported,
		upcloud.ErrCodeStorageTierIllegal:      vendors.Unsupported,
		// The zone cannot be used.
		upcloud.ErrCodeZoneNotFound: vendors.ZoneUnusable,
		upcloud.ErrCodeZoneInvalid:  vendors.ZoneUnusable,
		// Transient: concurrent creates, a stop or clone in progress, dependencies
		// that clear when the servers using a resource are gone.
		upcloud.ErrCodeServerCreatingLimitReached: vendors.Retryable,
		upcloud.ErrCodeServerStateIllegal:         vendors.Retryable,
		upcloud.ErrCodeStorageStateIllegal:        vendors.Retryable,
		upcloud.ErrCodeStorageInUse:               vendors.Retryable,
		upcloud.ErrCodeNetworkNotEmpty:            vendors.Retryable,
		codeRulesetInUse:                          vendors.Retryable,
		// Things that do not exist.
		upcloud.ErrCodeServerNotFound:  vendors.NotFound,
		upcloud.ErrCodeStorageNotFound: vendors.NotFound,
		upcloud.ErrCodeNetworkNotFound: vendors.NotFound,
		codeRulesetNotFound:            vendors.NotFound,
	}
	for _, c := range quotaCodes {
		m[c] = vendors.QuotaExceeded
	}
	return m
}()

// Classify wraps an error of an UpCloud call into the driver's error classes.
// Errors that are already classified pass through. The API's code is
// upcloud.Problem.ErrorCode() (the fragment of the problem type, or the bare
// legacy code); when it is not in the table, a 5xx status and 429 are Retryable
// and everything else is Fatal.
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
	}
	if p, ok := errors.AsType[*upcloud.Problem](err); ok {
		code := p.ErrorCode()
		if class, ok := errorClasses[code]; ok {
			e := vendors.NewError(class, code, err)
			if class == vendors.QuotaExceeded {
				e = e.AsAccountWide()
			}
			return e
		}
		switch {
		case p.Status == http.StatusTooManyRequests:
			return vendors.NewError(vendors.Retryable, CodeThrottled, err)
		case p.Status >= 500:
			return vendors.NewError(vendors.Retryable, CodeHTTP5xx, err)
		}
		return vendors.NewError(vendors.Fatal, code, err)
	}
	// An error answer the SDK could not parse (a proxy's HTML page on a 502, 503
	// or 429, say) arrives as the raw client error, wrapped.
	if ce, ok := errors.AsType[*client.Error](err); ok {
		switch {
		case ce.ErrorCode == http.StatusTooManyRequests:
			return vendors.NewError(vendors.Retryable, CodeThrottled, err)
		case ce.ErrorCode >= 500:
			return vendors.NewError(vendors.Retryable, CodeHTTP5xx, err)
		}
		return vendors.NewError(vendors.Fatal, "", err)
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return vendors.NewError(vendors.Retryable, CodeTransport, err)
	}
	return vendors.NewError(vendors.Fatal, "", err)
}

// classifyDelete is Classify for delete calls, where an unlisted code must not
// strand a resource: a 409 Conflict with an unlisted code is a dependency or a
// state that clears (a ruleset still attached, a network still in use, under
// whatever name the API gives it) and is Retryable. 404s are handled by isGone.
func classifyDelete(err error) error {
	if p, ok := errors.AsType[*upcloud.Problem](err); ok && p.Status == http.StatusConflict {
		if _, listed := errorClasses[p.ErrorCode()]; !listed {
			return vendors.NewError(vendors.Retryable, p.ErrorCode(), err)
		}
	}
	return Classify(err)
}

// isGone reports whether err says the thing does not exist: the NotFound class
// or, for the names the plan could not verify, an unlisted code with status 404.
// Deletes use it, so that a vanished server is success whatever the code is
// called.
func isGone(err error) bool {
	if vendors.IsClass(err, vendors.NotFound) {
		return true
	}
	if p, ok := errors.AsType[*upcloud.Problem](err); ok {
		// LABEL_LIMIT_REACHED is documented with status 404: not a gone thing.
		return p.Status == http.StatusNotFound && p.ErrorCode() != codeLabelLimit
	}
	return false
}
