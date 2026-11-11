/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package fake is an in-memory Azure Resource Manager for the Azure driver's
// tests: the virtual machine, network interface and public IP APIs, the SKU
// table, usages, marketplace images and agreements, locations, resource
// groups and tag listing, behind the interfaces of pkg/vendors/azure. It
// returns the SDK's *azcore.ResponseError values (built by the SDK's own
// runtime from an HTTP response), so the driver's classification runs on what
// it would see. It does not import the driver: tests wire it into
// azure.Clients.
//
//nolint:lll,goconst,dupl // the fake repeats the SDK's long signatures and ARM texts
package fake

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
)

// Error builds an ARM error as the SDK reports it: an *azcore.ResponseError with
// the status, the code and the message of the error body.
func Error(status int, code, message string) error {
	body := fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, code, message)
	resp := &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    &http.Request{Method: http.MethodPut, URL: &url.URL{Scheme: "https", Host: "management.azure.com", Path: "/fake"}},
	}
	return runtime.NewResponseError(resp)
}

// notFound is the 404 of a missing resource.
func notFound(code, what string) error {
	return Error(http.StatusNotFound, code, fmt.Sprintf("The %s was not found.", what))
}
