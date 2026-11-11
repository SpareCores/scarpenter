/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package v1alpha1_test

import (
	"fmt"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// The Hetzner Cloud block of the CRD (plan 15): the schema and the CEL rules of
// spec.vendors.hcloud and the hcloud blocks of the storage and placement
// profiles.

func TestHCloudSpecsAreAccepted(t *testing.T) {
	tests := map[string]string{
		"empty block": `vendors: {hcloud: {}}`,
		"everything": `
vendors:
  hcloud:
    regions: [fsn1, nbg1, hel1]
    networking: {publicIPv4: true, publicIPv6: false, inbound: {rules: []}}
    sshKey: my-ssh-key
    storage:
      data:
        - {name: pgdata, size: 100Gi, filesystem: xfs, mountPath: /var/lib/pgdata}
    resources:
      images: {use: {ids: {"*": ["123456789", "987654321"]}}}
      networks: {use: {ids: {fsn1: ["12345"], nbg1: ["12346"]}}}
      firewalls: {use: {tags: {scarpenter.sparecores.com/use: "true"}}}
      placementGroups: {create: {lifecycle: {policy: perUse}}}
profiles:
  storage: {db: {hcloud: {data: [{name: pgdata, size: 500Gi, filesystem: xfs, mountPath: /var/lib/pgdata}]}}}
  placement: {spread: {hcloud: {placementGroup: {type: spread}}}}`,
		"images by tags":          `vendors: {hcloud: {resources: {images: {use: {tags: {os: flatcar}, match: {architecture: [amd64]}}}}}}`,
		"networks by expression":  `vendors: {hcloud: {resources: {networks: {use: {expression: "env=prod,tier!=db"}}}}}`,
		"networks forbidden":      `vendors: {hcloud: {resources: {networks: {forbid: true}}}}`,
		"firewalls created":       `vendors: {hcloud: {resources: {firewalls: {create: {lifecycle: {policy: idleTTL, ttl: 24h}}}}}}`,
		"placement group by name": `profiles: {placement: {pg: {hcloud: {placementGroup: {use: {name: shared}}}}}}`,
		"vendor with aws":         `vendors: {aws: {}, hcloud: {}}`,
	}
	for name, spec := range tests {
		t.Run(name, func(t *testing.T) {
			if err := create(t, parseObject(t, "hcloud-valid-"+slug(name), withSpec(t, spec))); err != nil {
				t.Fatalf("create: %v", err)
			}
		})
	}
}

func TestHCloudInvalidSpecsAreRejected(t *testing.T) {
	tests := []struct{ name, spec, wantErr string }{
		{"networks create", `vendors: {hcloud: {resources: {networks: {create: {}}}}}`, "networks.create is not supported in v1"},
		{"networks use and create", `vendors: {hcloud: {resources: {networks: {use: {name: nm}, create: {}}}}}`, "at most one of use, create and forbid may be set"},
		{"images create", `vendors: {hcloud: {resources: {images: {create: {}}}}}`, "images accepts use only"},
		{"images forbid", `vendors: {hcloud: {resources: {images: {forbid: true}}}}`, "images accepts use only"},
		{"images select all", `vendors: {hcloud: {resources: {images: {use: {tags: {os: flatcar}, select: all}}}}}`, "images cannot use select all"},
		{"empty use", `vendors: {hcloud: {resources: {firewalls: {use: {}}}}}`, "set at least one of ids"},
		{"placement group without type or use", `profiles: {placement: {pg: {hcloud: {placementGroup: {}}}}}`, "set exactly one of type and use"},
		{"placement group with type and use", `profiles: {placement: {pg: {hcloud: {placementGroup: {type: spread, use: {name: x}}}}}}`, "set exactly one of type and use"},
		{"placement group type cluster", `profiles: {placement: {pg: {hcloud: {placementGroup: {type: cluster}}}}}`, "Unsupported value"},
		{"data volume without size", `vendors: {hcloud: {storage: {data: [{name: d, mountPath: /d}]}}}`, "Required value"},
		{"data volume without mountPath", `vendors: {hcloud: {storage: {data: [{name: d, size: 10Gi}]}}}`, "Required value"},
		{"duplicate data volume names", `vendors: {hcloud: {storage: {data: [{name: d, size: 10Gi, mountPath: /a}, {name: d, size: 10Gi, mountPath: /b}]}}}`, "Duplicate value"},
		{"root volume", `vendors: {hcloud: {storage: {root: {size: 64Gi}}}}`, "unknown field"},
		{"local storage", `vendors: {hcloud: {storage: {localStorage: {policy: mount}}}}`, "unknown field"},
		{"network profile", `profiles: {network: {n: {hcloud: {}}}}`, "unknown field"},
		{"unknown field", `vendors: {hcloud: {token: x}}`, "unknown field"},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := create(t, parseObject(t, fmt.Sprintf("hcloud-invalid-%d", i), withSpec(t, tc.spec)))
			if err == nil {
				t.Fatalf("object was accepted, want error containing %q", tc.wantErr)
			}
			if !apierrors.IsInvalid(err) && !apierrors.IsBadRequest(err) {
				t.Fatalf("want an Invalid or BadRequest error, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}
