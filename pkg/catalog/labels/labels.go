/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package labels turns the facts of the catalog into the label requirements of
// instance types and offerings (overview section 5.5, 00a sections 6.4 and 7):
// attribute labels, metric labels, selection membership, location labels and the
// profile labels. Every key is defined on every instance type, with
// DoesNotExist where a value is unknown or nothing applies: an undefined key
// would match every type (00a section 2).
package labels

import (
	"math"
	"regexp"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

// In returns a requirement that allows exactly the values.
func In(key string, values ...string) *scheduling.Requirement {
	return scheduling.NewRequirement(key, corev1.NodeSelectorOpIn, values...)
}

// Absent returns the requirement of a key that is not defined for the type or
// offering: DoesNotExist.
func Absent(key string) *scheduling.Requirement {
	return scheduling.NewRequirement(key, corev1.NodeSelectorOpDoesNotExist)
}

// Int is In with an integer value, rounded to the nearest.
func Int(key string, v float64) *scheduling.Requirement {
	return In(key, strconv.FormatInt(int64(math.Round(v)), 10))
}

// Bool is In with "true" or "false".
func Bool(key string, v bool) *scheduling.Requirement {
	return In(key, strconv.FormatBool(v))
}

var invalidValueChars = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// Value converts free text (a CPU family, a city) to a label value: characters
// outside the label-value charset become "-", the value starts and ends with
// an alphanumeric and has at most 63 characters. ok is false when nothing is
// left.
func Value(s string) (value string, ok bool) {
	v := invalidValueChars.ReplaceAllString(strings.TrimSpace(s), "-")
	if len(v) > 63 {
		v = v[:63]
	}
	v = strings.Trim(v, "-_.")
	return v, v != ""
}

// Enum is In with the sanitised value, or DoesNotExist when the text is empty.
func Enum(key, s string) *scheduling.Requirement {
	if v, ok := Value(s); ok {
		return In(key, v)
	}
	return Absent(key)
}

// Slug converts free text to a lower-case label-safe slug ("New York" becomes
// "new-york"), the form of the location labels.
func Slug(s string) (slug string, ok bool) {
	var b strings.Builder
	dash := true
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	slug = strings.Trim(b.String(), "-")
	if len(slug) > 63 {
		slug = strings.Trim(slug[:63], "-")
	}
	return slug, slug != ""
}

// Keys returns the keys of the requirements.
func Keys(reqs []*scheduling.Requirement) []string {
	out := make([]string, len(reqs))
	for i, r := range reqs {
		out[i] = r.Key
	}
	return out
}
