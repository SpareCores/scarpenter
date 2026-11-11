/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package sdk

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
)

// wrap turns a gophercloud error into an *osapi.APIError when the service
// answered with an HTTP status; transport and other errors pass through. The
// message is the service's: Nova's {"forbidden": {"message": ...}}, Neutron's
// {"NeutronError": {"type": ..., "message": ...}} (the type is prefixed, it is
// what tells OverQuota from a plain conflict), Glance's {"message": ...} or its
// plain text.
func wrap(service string, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*osapi.APIError](err); ok {
		return err
	}
	sc, ok := errors.AsType[statusCoder](err)
	if !ok {
		return err
	}
	text := err.Error()
	body := text
	if _, after, found := strings.Cut(text, "instead: "); found {
		body = after
	}
	return &osapi.APIError{Service: service, Status: sc.GetStatusCode(), Message: messageOf(body)}
}

// statusCoder is implemented by gophercloud's HTTP status errors.
type statusCoder interface {
	error
	GetStatusCode() int
}

// messageOf extracts the message of an OpenStack error body, the body itself
// when it is not JSON.
func messageOf(body string) string {
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return strings.TrimSpace(body)
	}
	if msg := findMessage(v); msg != "" {
		return msg
	}
	return strings.TrimSpace(body)
}

func findMessage(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	if msg, ok := m["message"].(string); ok {
		if t, ok := m["type"].(string); ok && t != "" {
			return t + ": " + msg
		}
		return msg
	}
	for _, inner := range m {
		if msg := findMessage(inner); msg != "" {
			return msg
		}
	}
	return ""
}

// noEndpoint reports whether the error is the catalog having no endpoint.
func noEndpoint(err error) bool {
	if _, ok := errors.AsType[*gophercloud.ErrEndpointNotFound](err); ok {
		return true
	}
	_, ok := errors.AsType[gophercloud.ErrEndpointNotFound](err)
	return ok
}
