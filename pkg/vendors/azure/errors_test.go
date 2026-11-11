/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/azure/fake"
)

// expected is an independent second copy of the plan's table (section 5): a
// change of errorClasses must be a change here too.
var expected = map[string]vendors.Class{
	"AllocationFailed":                         vendors.InsufficientCapacity,
	"ZonalAllocationFailed":                    vendors.InsufficientCapacity,
	"OverconstrainedAllocationRequest":         vendors.InsufficientCapacity,
	"OverconstrainedZonalAllocationRequest":    vendors.InsufficientCapacity,
	"AzureSpotIsNotSupportedForThisVMSize":     vendors.Unsupported,
	"ZoneNotAvailableForSubscription":          vendors.ZoneUnusable,
	"QuotaExceeded":                            vendors.QuotaExceeded,
	"PublicIPCountLimitReached":                vendors.QuotaExceeded,
	"TooManyRequests":                          vendors.Retryable,
	"InternalServerError":                      vendors.Retryable,
	"ServerBusy":                               vendors.Retryable,
	"RetryableError":                           vendors.Retryable,
	"ReferencedResourceNotProvisioned":         vendors.Retryable,
	"AnotherOperationInProgress":               vendors.Retryable,
	"OSProvisioningTimedOut":                   vendors.Retryable,
	"InUseSubnetCannotBeDeleted":               vendors.Retryable,
	"NetworkSecurityGroupInUse":                vendors.Retryable,
	"PublicIPAddressCannotBeDeleted":           vendors.Retryable,
	"InUseNetworkSecurityGroupCannotBeDeleted": vendors.Retryable,
	"ScopeLocked":                              vendors.Retryable,
	"ApplicationSecurityGroupInUse":            vendors.Retryable,
	"ProximityPlacementGroupInUse":             vendors.Retryable,
	"ResourceNotFound":                         vendors.NotFound,
	"ResourceGroupNotFound":                    vendors.NotFound,
	"VMNotFound":                               vendors.NotFound,
	"PlatformImageNotFound":                    vendors.NotFound,
	// The plan's Fatal list.
	"AuthorizationFailed":                  vendors.Fatal,
	"LinkedAuthorizationFailed":            vendors.Fatal,
	"MissingSubscriptionRegistration":      vendors.Fatal,
	"VMMarketplaceInvalidInput":            vendors.Fatal,
	"MarketplacePurchaseEligibilityFailed": vendors.Fatal,
	"ReadOnlyDisabledSubscription":         vendors.Fatal,
	"InvalidParameter":                     vendors.Fatal,
	"UserDataTooLarge":                     vendors.Fatal,
	"SomethingNobodyKnows":                 vendors.Fatal,
}

func TestClassifyCodes(t *testing.T) {
	for code, want := range expected {
		t.Run(code, func(t *testing.T) {
			// A status that would classify differently proves the code decides.
			err := classify(fake.Error(http.StatusBadRequest, code, "message"), errContext{size: "Standard_D2s_v5"})
			require.Equal(t, want, vendors.ClassOf(err), "%v", err)
			require.Equal(t, code, vendors.CodeOf(err))
		})
	}
	// Every code of the table is in the independent copy.
	for code, class := range errorClasses {
		require.Equal(t, expected[code], class, code)
	}
}

func TestClassifySkuNotAvailableDependsOnPriority(t *testing.T) {
	err := fake.Error(http.StatusBadRequest, "SkuNotAvailable", "not available")
	require.Equal(t, vendors.Unsupported, vendors.ClassOf(classify(err, errContext{})))
	require.Equal(t, vendors.InsufficientCapacity, vendors.ClassOf(classify(err, errContext{spot: true})))
}

func TestClassifyMessageExceptions(t *testing.T) {
	cases := []struct {
		name, code, msg string
		ec              errContext
		want            vendors.Class
	}{
		{"quota text", "OperationNotAllowed", "Operation could not be completed as it results in exceeding approved Total Regional Cores quota.", errContext{}, vendors.QuotaExceeded},
		{"in progress", "OperationNotAllowed", "Another operation is in progress on the resource.", errContext{}, vendors.Retryable},
		{"other not allowed", "OperationNotAllowed", "The subscription is disabled.", errContext{}, vendors.Fatal},
		{"size not available", "InvalidParameter", "The requested VM size Standard_X1 is not available in the current region.", errContext{size: "Standard_X1"}, vendors.Unsupported},
		{"size not supported", "InvalidParameter", "VM size Standard_X1 is not supported in this zone.", errContext{size: "Standard_X1"}, vendors.Unsupported},
		{"another size", "InvalidParameter", "The requested VM size Standard_X2 is not available.", errContext{size: "Standard_X1"}, vendors.Fatal},
		{"plain invalid", "InvalidParameter", "Parameter x is wrong.", errContext{size: "Standard_X1"}, vendors.Fatal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classify(fake.Error(http.StatusBadRequest, c.code, c.msg), c.ec)
			require.Equal(t, c.want, vendors.ClassOf(got), "%v", got)
		})
	}
}

func TestClassifyTransportAndStatus(t *testing.T) {
	for status, want := range map[int]vendors.Class{
		http.StatusNotFound: vendors.NotFound, http.StatusTooManyRequests: vendors.Retryable,
		http.StatusInternalServerError: vendors.Retryable, http.StatusBadGateway: vendors.Retryable,
		http.StatusBadRequest: vendors.Fatal, http.StatusForbidden: vendors.Fatal,
	} {
		require.Equal(t, want, vendors.ClassOf(classify(fake.Error(status, "", "x"), errContext{})), "status %d", status)
	}
	// A 429 is throttling whatever the code: the compute provider's is
	// OperationNotAllowed, ARM's SubscriptionRequestsThrottled.
	for _, code := range []string{"OperationNotAllowed", "SubscriptionRequestsThrottled", "InvalidParameter", "SkuNotAvailable"} {
		got := classify(fake.Error(http.StatusTooManyRequests, code,
			"The server rejected the request because too many requests have been received for this subscription."),
			errContext{size: "Standard_D2s_v5"})
		require.Equal(t, vendors.Retryable, vendors.ClassOf(got), code)
		require.Equal(t, code, vendors.CodeOf(got))
	}
	require.Equal(t, vendors.Retryable, vendors.ClassOf(classify(context.DeadlineExceeded, errContext{})))
	require.Equal(t, codeTimeout, vendors.CodeOf(classify(fmt.Errorf("wrapped: %w", context.DeadlineExceeded), errContext{})))
	require.Equal(t, codeCanceled, vendors.CodeOf(classify(context.Canceled, errContext{})))
	require.Equal(t, vendors.Retryable, vendors.ClassOf(classify(&net.DNSError{IsTimeout: true}, errContext{})))
	require.Equal(t, vendors.Fatal, vendors.ClassOf(classify(errors.New("boom"), errContext{})))
	require.NoError(t, classify(nil, errContext{}))
	// Already classified errors pass through.
	pre := vendors.NewError(vendors.QuotaExceeded, "X", errors.New("x"))
	require.Same(t, pre, classify(pre, errContext{}))
	// The account-wide flag is for vendors with account limits: Azure's are per region.
	require.False(t, vendors.IsAccountWide(classify(fake.Error(409, "QuotaExceeded", "x"), errContext{})))
}

func TestClassifyRegionRead(t *testing.T) {
	for _, code := range []string{"LocationNotAvailableForResourceType", "InvalidResourceLocation", "NotAvailableForSubscription", "AuthorizationFailed"} {
		require.Equal(t, vendors.RegionUnavailable, vendors.ClassOf(classifyRegionRead(fake.Error(403, code, "x"))), code)
	}
	// Launch calls keep AuthorizationFailed Fatal.
	require.Equal(t, vendors.Fatal, vendors.ClassOf(classify(fake.Error(403, "AuthorizationFailed", "x"), errContext{})))
	require.Equal(t, vendors.Retryable, vendors.ClassOf(classifyRegionRead(fake.Error(429, "TooManyRequests", "x"))))
	require.NoError(t, classifyRegionRead(nil))
}

func TestIsNotFound(t *testing.T) {
	require.True(t, isNotFound(fake.Error(404, "", "x")))
	require.True(t, isNotFound(fake.Error(400, "PlatformImageNotFound", "x")))
	require.True(t, isNotFound(vendors.Errorf(vendors.NotFound, "x")))
	require.False(t, isNotFound(fake.Error(400, "InvalidParameter", "x")))
	require.False(t, isNotFound(nil))
}
