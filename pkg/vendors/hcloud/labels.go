/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/
package hcloud

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// LabelCapacityType is the label every server carries with its capacity type
// (always on-demand: Hetzner has no spot).
const LabelCapacityType = "scarpenter.sparecores.com/capacity-type"

// Limits of Hetzner labels and names (plan 15, "Resource kinds").
const (
	maxLabelValue = 63
	// shortPrefix is how much of a value that does not fit survives; with the
	// hyphen and the 8-character hash it makes 59 characters.
	shortPrefix = 50
	hashLen     = 8
	// MaxServerName is the longest server name (RFC 1123 label).
	MaxServerName = 63
	// MaxVolumeName is the longest volume name.
	MaxVolumeName = 64
)

var (
	labelValueRE = regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?)?$`)
	labelNameRE  = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)
	dnsPrefixRE  = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
	hostLabelRE  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	invalidRunRE = regexp.MustCompile(`[^A-Za-z0-9._-]`)
)

// TagKey maps a logical tag key to the label key: scarpenter.sparecores.com/<key>,
// a valid Hetzner key (DNS prefix plus name).
func TagKey(k vendors.TagKey) string { return vendors.DefaultTagKey(k) }

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:hashLen]
}

// LabelValue maps a value to one Hetzner accepts (at most 63 characters of
// [A-Za-z0-9._-], starting and ending alphanumeric): a value that already fits
// is unchanged; any other becomes its first 50 valid characters, "-" and an
// 8-character hash of the whole value. The same function runs on write and on
// lookup, so a lookup by the long name finds the label. Two long names with the
// same prefix differ in the hash (a collision needs the first 32 bits of
// SHA-256 to agree).
func LabelValue(v string) string {
	if len(v) <= maxLabelValue && labelValueRE.MatchString(v) {
		return v
	}
	return shorten(v, shortPrefix)
}

// shorten is the prefix-and-hash form of v with at most n prefix characters of
// the characters Hetzner allows.
func shorten(v string, n int) string {
	p := invalidRunRE.ReplaceAllString(v, "-")
	if len(p) > n {
		p = p[:n]
	}
	p = strings.TrimFunc(p, func(r rune) bool { return !isAlnum(r) })
	if p == "" {
		return shortHash(v)
	}
	return p + "-" + shortHash(v)
}

func isAlnum(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// ValidLabelKey reports whether k is a valid Hetzner label key: an optional DNS
// prefix (at most 253 characters) and "/", then a name of at most 63 characters.
func ValidLabelKey(k string) bool {
	prefix, name, hasPrefix := strings.Cut(k, "/")
	if !hasPrefix {
		name, prefix = k, ""
	}
	if len(name) > 63 || !labelNameRE.MatchString(name) {
		return false
	}
	if hasPrefix && (len(prefix) > 253 || !dnsPrefixRE.MatchString(prefix)) {
		return false
	}
	return true
}

// Labels builds the labels of a created resource: the logical tags through
// TagKey and LabelValue, plus the NodeClass's own tags (extra keys must be valid
// label keys; values go through LabelValue). A logical tag wins over an extra of
// the same key.
func Labels(tags map[vendors.TagKey]string, extra map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(tags)+len(extra))
	for k, v := range extra {
		if !ValidLabelKey(k) {
			return nil, fmt.Errorf("the tag key %q is not a valid Hetzner label key", k)
		}
		out[k] = LabelValue(v)
	}
	for k, v := range tags {
		out[TagKey(k)] = LabelValue(v)
	}
	return out, nil
}

// Selector builds the label selector that matches resources carrying all the
// tags (equality terms, sorted, "a=b,c=d"). An empty set is an empty selector.
func Selector(tags map[vendors.TagKey]string) string {
	terms := make([]string, 0, len(tags))
	for k, v := range tags {
		terms = append(terms, TagKey(k)+"="+LabelValue(v))
	}
	slices.Sort(terms)
	return strings.Join(terms, ",")
}

// KeyPresent is the selector term that matches a label key whatever its value.
func KeyPresent(k vendors.TagKey) string { return TagKey(k) }

// TagsOf reads the logical tags back from a resource's labels. Values that were
// shortened on write come back shortened (the original is not recoverable; the
// same function maps lookups).
func TagsOf(labels map[string]string) map[vendors.TagKey]string {
	out := map[vendors.TagKey]string{}
	for _, k := range vendors.TagKeys {
		if v, ok := labels[TagKey(k)]; ok {
			out[k] = v
		}
	}
	if v, ok := labels[TagKey(vendors.TagResource)]; ok {
		out[vendors.TagResource] = v
	}
	return out
}

// HasTags reports whether labels carry all the tags (values mapped like
// Selector), for the client-side filter of a shared list.
func HasTags(labels map[string]string, tags map[vendors.TagKey]string) bool {
	for k, v := range tags {
		if got, ok := labels[TagKey(k)]; !ok || got != LabelValue(v) {
			return false
		}
	}
	return true
}

// ServerName maps a NodeClaim name to a server name: unchanged when it is a
// valid RFC 1123 label of at most 63 characters, else the first 50 characters, "-" and an
// 8-character hash of the full name. The server name is unique per project, which
// is the idempotency of a create (together with the idempotency-key label).
func ServerName(nodeClaim string) string {
	return fitName(nodeClaim, MaxServerName)
}

// ResourceName maps a deterministic resource name (scarpenter-<cluster>-<kind>-<region>[-hash])
// to one of at most max characters (use MaxServerName or MaxVolumeName).
func ResourceName(name string, max int) string {
	return fitName(name, max)
}

func fitName(name string, max int) string {
	if len(name) <= max && hostLabelRE.MatchString(name) {
		return name
	}
	n := min(shortPrefix, max-1-hashLen)
	p := strings.ToLower(invalidRunRE.ReplaceAllString(name, "-"))
	p = strings.ReplaceAll(strings.ReplaceAll(p, ".", "-"), "_", "-")
	if len(p) > n {
		p = p[:n]
	}
	p = strings.Trim(p, "-")
	if p == "" {
		return shortHash(name)
	}
	return p + "-" + shortHash(name)
}
