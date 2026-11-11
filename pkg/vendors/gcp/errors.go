/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/googleapi"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Error codes the driver itself raises (the vendor codes are Compute's).
const (
	codeTimeout     = "CallTimeout"
	codeCanceled    = "CallCanceled"
	codeTransport   = "TransportError"
	codeServerError = "ServerError"
	codeHTTPPrefix  = "HTTP"
	// CodeOrgPolicy is the code of a launch refused by an organization policy
	// (CONDITION_NOT_MET); the message names the constraint when GCP does.
	CodeOrgPolicy = "OrganizationPolicyViolation"
	// CodeInvalidName is raised for a malformed reference or name.
	CodeInvalidName = "InvalidName"
)

// Operation error codes (operation.error.errors[].code) and the reasons of HTTP
// errors that the driver acts on outside the classification table.
const (
	CodeAlreadyExists  = "ALREADY_EXISTS"
	CodeConditionNot   = "CONDITION_NOT_MET"
	CodeNotFound       = "NOT_FOUND"
	CodeQuotaExceeded  = "QUOTA_EXCEEDED"
	CodeUnsupportedOp  = "UNSUPPORTED_OPERATION"
	reasonAlreadyExist = "alreadyExists"
	reasonConditionNot = "conditionNotMet"
	reasonInUse        = "resourceInUseByAnotherResource"
	codeInUse          = "RESOURCE_IN_USE_BY_ANOTHER_RESOURCE"
	reasonNotFound     = "notFound"
	reasonQuota        = "quotaExceeded"
)

// errorClasses maps the operation error codes and the reasons of HTTP errors to
// the driver's classes (plan 16, section 5). It is the single table: classify
// reads it and a test covers every entry. Every entry is unverified live (the
// spike and the first lane run confirm them): the codes come from Google's
// resource availability and quota troubleshooting pages and from the API's
// reason vocabulary.
var errorClasses = map[string]vendors.Class{
	// Stock: the zone or region has no capacity for the type right now.
	"ZONE_RESOURCE_POOL_EXHAUSTED":                vendors.InsufficientCapacity,
	"ZONE_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS":   vendors.InsufficientCapacity,
	"REGION_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS": vendors.InsufficientCapacity,
	"POOL_CAPACITY_INSUFFICIENT":                  vendors.InsufficientCapacity,
	// A limit of the project: the metric is in the message.
	CodeQuotaExceeded: vendors.QuotaExceeded,
	// The type or the configuration is not offered where it was asked for.
	CodeUnsupportedOp: vendors.Unsupported,
	// The zone cannot host the instance: the subnet is full.
	"IP_SPACE_EXHAUSTED":              vendors.ZoneUnusable,
	"IP_SPACE_EXHAUSTED_WITH_DETAILS": vendors.ZoneUnusable,
	// Transient.
	"RESOURCE_OPERATION_RATE_EXCEEDED": vendors.Retryable,
	"INTERNAL_ERROR":                   vendors.Retryable,
	"SERVICE_UNAVAILABLE":              vendors.Retryable,
	"backendError":                     vendors.Retryable,
	"rateLimitExceeded":                vendors.Retryable,
	"userRateLimitExceeded":            vendors.Retryable,
	"resourceOperationRateExceeded":    vendors.Retryable,
	"internalError":                    vendors.Retryable,
	// A resource still used by another one (a subnet with instances, a policy
	// attached to an instance): clears when the users are gone, like AWS's
	// DependencyViolation.
	reasonInUse: vendors.Retryable,
	codeInUse:   vendors.Retryable,
	// Things that do not exist.
	reasonNotFound: vendors.NotFound,
	// Fatal on purpose, listed so that the table documents them.
	CodeAlreadyExists:  vendors.Fatal,
	reasonAlreadyExist: vendors.Fatal,
	CodeConditionNot:   vendors.Fatal,
	reasonConditionNot: vendors.Fatal,
}

// errContext is what the classification of one call knows about it.
type errContext struct {
	machineType, zone string
}

var quotaMetricRe = regexp.MustCompile(`Quota '([A-Za-z0-9_]+)' exceeded`)

// QuotaMetricOf extracts the metric from the message of a QUOTA_EXCEEDED error
// ("Quota 'CPUS' exceeded. Limit: 24.0 in region europe-west3."); "" when the
// message has another form. The quota tracker refreshes the region's limits
// when it sees one.
func QuotaMetricOf(msg string) string {
	if m := quotaMetricRe.FindStringSubmatch(msg); m != nil {
		return m[1]
	}
	return ""
}

// accountWideMetric reports whether the metric limits the whole project: the
// "all regions" quotas. Other project-wide metrics (networks, firewalls) do not
// stop a launch in every region.
func accountWideMetric(metric string) bool {
	return strings.HasSuffix(metric, "_ALL_REGIONS")
}

// opErr is one error of a finished operation or of an HTTP error response.
type opErr struct {
	code, message string
	status        int
}

// classifyOpErr classifies one error entry.
func classifyOpErr(e opErr, ec errContext, cause error) error {
	code := e.code
	// A machine type that does not exist in the zone is Unsupported, not a
	// missing resource. The insert's HTTP answer may also be a 400 "invalid"
	// ("Invalid value for field 'resource.machineType': ... does not exist in
	// zone"; UNVERIFIED live which of the two forms GCE sends).
	if ec.machineType != "" && strings.Contains(e.message, "machineTypes/"+ec.machineType) &&
		(code == CodeNotFound || code == reasonNotFound || (code == "" && e.status == http.StatusNotFound) ||
			strings.Contains(e.message, "does not exist")) {
		return vendors.NewError(vendors.Unsupported, CodeNotFound, cause)
	}
	switch code {
	case CodeNotFound:
		return vendors.NewError(vendors.NotFound, code, cause)
	case CodeConditionNot, reasonConditionNot:
		return vendors.NewError(vendors.Fatal, CodeOrgPolicy, fmt.Errorf(
			"%w (an organization policy such as constraints/compute.trustedImageProjects may forbid the request)", cause))
	case CodeQuotaExceeded:
		err := vendors.NewError(vendors.QuotaExceeded, code, cause)
		if accountWideMetric(QuotaMetricOf(e.message)) {
			return err.AsAccountWide()
		}
		return err
	}
	if class, ok := errorClasses[code]; ok {
		return vendors.NewError(class, code, cause)
	}
	// reason "quotaExceeded" on an HTTP error is a resource quota; the API's own
	// request rate limits use rateLimitExceeded.
	if code == reasonQuota {
		err := vendors.NewError(vendors.QuotaExceeded, CodeQuotaExceeded, cause)
		if accountWideMetric(QuotaMetricOf(e.message)) {
			return err.AsAccountWide()
		}
		return err
	}
	switch {
	case e.status == http.StatusTooManyRequests:
		return vendors.NewError(vendors.Retryable, codeHTTPPrefix+"429", cause)
	case e.status >= 500:
		return vendors.NewError(vendors.Retryable, codeServerError, cause)
	case e.status == http.StatusNotFound:
		return vendors.NewError(vendors.NotFound, codeHTTPPrefix+"404", cause)
	}
	if code == "" && e.status != 0 {
		code = fmt.Sprintf("%s%d", codeHTTPPrefix, e.status)
	}
	return vendors.NewError(vendors.Fatal, code, cause)
}

// classify wraps an error of a Compute call into the driver's error classes.
// Errors that are already classified pass through unchanged. Everything not
// listed is Fatal (permission denied, invalid arguments, ...).
func classify(err error, ec errContext) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*vendors.Error](err); ok {
		return err
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return vendors.NewError(vendors.Retryable, codeTimeout, err)
	case errors.Is(err, context.Canceled):
		return vendors.NewError(vendors.Retryable, codeCanceled, err)
	}
	if ge, ok := errors.AsType[*googleapi.Error](err); ok {
		// The first item with a known reason decides; the status is the fallback.
		for _, item := range ge.Errors {
			if _, known := errorClasses[item.Reason]; known || item.Reason == reasonQuota {
				return classifyOpErr(opErr{code: item.Reason, message: ge.Message, status: ge.Code}, ec, err)
			}
		}
		return classifyOpErr(opErr{message: ge.Message, status: ge.Code}, ec, err)
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return vendors.NewError(vendors.Retryable, codeTransport, err)
	}
	return vendors.NewError(vendors.Fatal, "", err)
}

// classifyOperation returns nil for an operation that finished without error and
// the classified first error of one that failed (capacity and quota errors
// appear in the finished operation, not in the HTTP response of the insert).
// An operation that is not DONE is an error of the caller, not a result.
func classifyOperation(op *computepb.Operation, ec errContext) error {
	if op == nil {
		return vendors.Errorf(vendors.Fatal, "no operation returned")
	}
	if op.GetStatus() != computepb.Operation_DONE {
		return vendors.NewError(vendors.Retryable, codeTimeout,
			fmt.Errorf("operation %s is %s", op.GetName(), op.GetStatus()))
	}
	if op.GetError() == nil || len(op.GetError().GetErrors()) == 0 {
		if op.GetHttpErrorStatusCode() >= 400 {
			e := opErr{message: op.GetHttpErrorMessage(), status: int(op.GetHttpErrorStatusCode())}
			return classifyOpErr(e, ec, fmt.Errorf("operation %s failed: HTTP %d %s", op.GetName(), e.status, e.message))
		}
		return nil
	}
	first := op.GetError().GetErrors()[0]
	e := opErr{code: first.GetCode(), message: first.GetMessage(), status: int(op.GetHttpErrorStatusCode())}
	return classifyOpErr(e, ec,
		fmt.Errorf("operation %s failed: %s: %s", op.GetName(), first.GetCode(), first.GetMessage()))
}

// regionDeniedStatus are the HTTP statuses that say the credentials cannot read
// a region.
var regionDeniedStatus = []int{http.StatusUnauthorized, http.StatusForbidden}

// classifyRegionRead classifies the error of a call that reads the facts of a
// region (regions.get, zones.list): a permission error is RegionUnavailable, so
// that the facts skip the region; the rest is classify's.
func classifyRegionRead(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*vendors.Error](err); !ok {
		ge, ok := errors.AsType[*googleapi.Error](err)
		if ok && containsInt(regionDeniedStatus, ge.Code) && !isQuotaReason(ge) {
			return vendors.NewError(vendors.RegionUnavailable, fmt.Sprintf("%s%d", codeHTTPPrefix, ge.Code), err)
		}
	}
	return classify(err, errContext{})
}

func isQuotaReason(ge *googleapi.Error) bool {
	for _, i := range ge.Errors {
		if i.Reason == "rateLimitExceeded" || i.Reason == "userRateLimitExceeded" || i.Reason == reasonQuota {
			return true
		}
	}
	return false
}

func containsInt(s []int, v int) bool {
	return slices.Contains(s, v)
}

// IsAlreadyExists reports whether err is the 409 of a name that is taken
// (before or after classification).
func IsAlreadyExists(err error) bool {
	if ge, ok := errors.AsType[*googleapi.Error](err); ok {
		if ge.Code == http.StatusConflict {
			return true
		}
		for _, i := range ge.Errors {
			if i.Reason == reasonAlreadyExist {
				return true
			}
		}
	}
	return vendors.CodeOf(err) == CodeAlreadyExists || vendors.CodeOf(err) == reasonAlreadyExist
}

// IsNotFound reports whether err says a resource does not exist.
func IsNotFound(err error) bool {
	if vendors.IsClass(err, vendors.NotFound) {
		return true
	}
	ge, ok := errors.AsType[*googleapi.Error](err)
	return ok && ge.Code == http.StatusNotFound
}
