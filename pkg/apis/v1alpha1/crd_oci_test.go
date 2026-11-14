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

// The OCI block of the CRD (plan 25): the schema and the CEL rules of
// spec.vendors.oci. OCI has no profile blocks in v1.

func TestOCISpecsAreAccepted(t *testing.T) {
	tests := map[string]string{
		"empty block": `vendors: {oci: {}}`,
		"everything": `
vendors:
  oci:
    regions: [eu-frankfurt-1, us-ashburn-1]
    networking: {publicIPv4: true, publicIPv6: false, inbound: {rules: []}}
    storage: {root: {sizeGb: 100, performance: ultra-high-performance, vpusPerGb: 40, kmsKeyId: ocid1.key.oc1.eu-frankfurt-1.aaaa}}
    metadata: {custom: value}
    resources:
      subnets: {use: {ids: {eu-frankfurt-1: [ocid1.subnet.oc1.eu-frankfurt-1.aaaa]}}}
      networkSecurityGroups: {use: {ids: {eu-frankfurt-1: [ocid1.networksecuritygroup.oc1.eu-frankfurt-1.aaaa]}}}
      images: {use: {ids: {eu-frankfurt-1: [ocid1.image.oc1.eu-frankfurt-1.aaaa], us-ashburn-1: [ocid1.image.oc1.iad.aaaa]}}}`,
		"subnets created with a range": `vendors: {oci: {resources: {subnets: {create: {cidr: 172.20.0.0/16, lifecycle: {policy: retain}}}}}}`,
		"subnets by tags":              `vendors: {oci: {resources: {subnets: {use: {tags: {scarpenter-use: "true"}, match: {within: 10.0.0.0/8}}}}}}`,
		"groups created":               `vendors: {oci: {resources: {networkSecurityGroups: {create: {lifecycle: {policy: idleTTL, ttl: 24h}}}}}}`,
		"groups forbidden":             `vendors: {oci: {resources: {networkSecurityGroups: {forbid: true}}}}`,
		"images by name":               `vendors: {oci: {resources: {images: {use: {name: "flatcar-{channel}-{version}-{arch}"}}}}}`,
		"vendor with aws":              `vendors: {aws: {}, oci: {}}`,
	}
	for name, spec := range tests {
		t.Run(name, func(t *testing.T) {
			if err := create(t, parseObject(t, "oci-valid-"+slug(name), withSpec(t, spec))); err != nil {
				t.Fatalf("create: %v", err)
			}
		})
	}
}

func TestOCIInvalidSpecsAreRejected(t *testing.T) {
	tests := []struct{ name, spec, wantErr string }{
		{"region name", `vendors: {oci: {regions: [Frankfurt]}}`, "should match"},
		{"subnets use and create", `vendors: {oci: {resources: {subnets: {use: {name: a}, create: {}}}}}`, "at most one of use, create and forbid may be set"},
		{"groups use and create", `vendors: {oci: {resources: {networkSecurityGroups: {use: {name: a}, create: {}}}}}`, "at most one of use, create and forbid may be set"},
		{"subnet ids with *", `vendors: {oci: {resources: {subnets: {use: {ids: {"*": [ocid1.subnet.oc1.eu-frankfurt-1.aaaa]}}}}}}`, "never *"},
		{"subnet id not an OCID", `vendors: {oci: {resources: {subnets: {use: {ids: {eu-frankfurt-1: [subnet-123]}}}}}}`, "subnet OCIDs"},
		{"subnet id of another kind", `vendors: {oci: {resources: {subnets: {use: {ids: {eu-frankfurt-1: [ocid1.image.oc1.eu-frankfurt-1.aaaa]}}}}}}`, "subnet OCIDs"},
		{"group id not an OCID", `vendors: {oci: {resources: {networkSecurityGroups: {use: {ids: {eu-frankfurt-1: [nsg]}}}}}}`, "network security group OCIDs"},
		{"image ids with *", `vendors: {oci: {resources: {images: {use: {ids: {"*": [ocid1.image.oc1.eu-frankfurt-1.aaaa]}}}}}}`, "never *"},
		{"image id not an OCID", `vendors: {oci: {resources: {images: {use: {ids: {eu-frankfurt-1: [flatcar]}}}}}}`, "image OCIDs"},
		{"images create", `vendors: {oci: {resources: {images: {create: {}}}}}`, "images accepts use only"},
		{"images forbid", `vendors: {oci: {resources: {images: {forbid: true}}}}`, "images accepts use only"},
		{"images select all", `vendors: {oci: {resources: {images: {use: {tags: {os: flatcar}, select: all}}}}}`, "images cannot use select all"},
		{"empty use", `vendors: {oci: {resources: {subnets: {use: {}}}}}`, "set at least one of ids"},
		{"subnet range not private", `vendors: {oci: {resources: {subnets: {create: {cidr: 8.8.0.0/16}}}}}`, "cidr must be an IPv4 CIDR"},
		{"subnet range too small", `vendors: {oci: {resources: {subnets: {create: {cidr: 10.0.0.0/24}}}}}`, "cidr must be an IPv4 CIDR"},
		{"root size too small", `vendors: {oci: {storage: {root: {sizeGb: 20}}}}`, "should be greater than or equal to 50"},
		{"root performance", `vendors: {oci: {storage: {root: {performance: fast}}}}`, "Unsupported value"},
		{"vpus without ultra", `vendors: {oci: {storage: {root: {performance: balanced, vpusPerGb: 40}}}}`, "vpusPerGb is only valid"},
		{"vpus not a multiple of 10", `vendors: {oci: {storage: {root: {performance: ultra-high-performance, vpusPerGb: 35}}}}`, "multiple of 10"},
		{"kms key", `vendors: {oci: {storage: {root: {kmsKeyId: mykey}}}}`, "should match"},
		{"metadata user_data", `vendors: {oci: {metadata: {user_data: abc}}}`, "reserved"},
		{"metadata ssh keys", `vendors: {oci: {metadata: {ssh_authorized_keys: abc}}}`, "reserved"},
		{"data volumes", `vendors: {oci: {storage: {data: [{name: d, size: 10Gi, mountPath: /d}]}}}`, "unknown field"},
		{"local storage", `vendors: {oci: {storage: {localStorage: {policy: mount}}}}`, "unknown field"},
		{"compartment", `vendors: {oci: {compartmentId: ocid1.compartment.oc1..aaaa}}`, "unknown field"},
		{"storage profile", `profiles: {storage: {s: {oci: {}}}}`, "unknown field"},
		{"unknown field", `vendors: {oci: {token: x}}`, "unknown field"},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := create(t, parseObject(t, fmt.Sprintf("oci-invalid-%d", i), withSpec(t, tc.spec)))
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
