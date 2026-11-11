/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package alicloud is the foundation of the Alibaba Cloud (ECS and VPC) driver
// of plan 21: the SDK clients behind narrow interfaces, error classification,
// the pure mappings (names, provider IDs, states, the launch request), the
// instance operations and the quota reader. The vendors.Driver itself, its
// registration and the NodeClass API types follow plan 14.
package alicloud

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/SpareCores/scarpenter/pkg/scope"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Vendor is the Keeper vendor ID and the registration name.
const Vendor = "alicloud"

// DefaultHomeRegion is where the global DescribeRegions is read
// (--alicloud-home-region).
const DefaultHomeRegion = "ap-southeast-1"

// ProviderID is scarpenter://alicloud/<region>/<nodeclaim>.
func ProviderID(region, nodeClaim string) string {
	return scope.ProviderID(Vendor, region, nodeClaim)
}

// ParseProviderID splits an Alibaba Cloud provider ID into region and NodeClaim
// name; it fails for another vendor's ID.
func ParseProviderID(id string) (region, nodeClaim string, err error) {
	vendor, region, nodeClaim, err := scope.ParseProviderID(id)
	if err != nil {
		return "", "", err
	}
	if vendor != Vendor {
		return "", "", fmt.Errorf("provider ID %q is not of vendor %s", id, Vendor)
	}
	return region, nodeClaim, nil
}

// TagKey maps a logical tag key to ECS's charset: slash keys are valid (no
// aliyun or acs: prefix, no http://), so the AWS-style
// scarpenter.sparecores.com/<key> is used.
func TagKey(k vendors.TagKey) string { return vendors.DefaultTagKey(k) }

// logicalKey is TagKey's inverse for the keys the driver writes; ok is false for
// any other tag (the NodeClass's own).
func logicalKey(key string) (vendors.TagKey, bool) {
	for _, k := range vendors.TagKeys {
		if TagKey(k) == key {
			return k, true
		}
	}
	return "", false
}

// Limits of the names ECS accepts (plan 21 section 3).
const (
	maxInstanceName = 128
	maxHostName     = 64
	hashLen         = 8
)

// HostName is the host name of a NodeClaim: the name itself, or, when it is
// longer than the 64 characters ECS accepts, a shortened prefix and a hash of
// the full name (the instance name keeps the whole NodeClaim name, up to its 128
// characters).
func HostName(nodeClaim string) string {
	if len(nodeClaim) <= maxHostName {
		return nodeClaim
	}
	sum := sha256.Sum256([]byte(nodeClaim))
	keep := maxHostName - hashLen - 1
	return strings.TrimRight(nodeClaim[:keep], ".-") + "-" + hex.EncodeToString(sum[:])[:hashLen]
}

// InstanceName is the ECS instance name of a NodeClaim (2 to 128 characters).
func InstanceName(nodeClaim string) (string, error) {
	if len(nodeClaim) < 2 || len(nodeClaim) > maxInstanceName {
		return "", fmt.Errorf("the NodeClaim name %q is not 2 to %d characters", nodeClaim, maxInstanceName)
	}
	return nodeClaim, nil
}

// ClientToken is the idempotency token of a launch: the hex SHA-256 of the
// idempotency key and of everything else a replay may change (region, zone,
// type, capacity type, image, vSwitch), so that two launches that differ in
// any of them never share a token (ECS answers a reused token with other
// parameters with IdempotentParameterMismatch). attempt salts the token of the
// retry after such an answer when the instance of the first token cannot be
// found. 64 hex characters: the limit of ClientToken.
func ClientToken(key, region, zone, instanceType, capacityType, image, vSwitch string, attempt int) string {
	h := sha256.New()
	for _, p := range []string{key, region, zone, instanceType, capacityType, image, vSwitch, fmt.Sprint(attempt)} {
		_, _ = fmt.Fprintf(h, "%d:%s|", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))
}
