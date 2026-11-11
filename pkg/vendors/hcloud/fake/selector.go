/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/
package fake

import (
	"fmt"
	"slices"
	"strings"
)

// term is one requirement of a label selector.
type term struct {
	key    string
	op     string // =, !=, in, notin, exists, notexists
	values []string
}

// Selector is a parsed label selector, the language of the API's
// label_selector parameter: equality (k=v, k==v), inequality (k!=v), set (k in
// (a,b), k notin (a,b)), presence (k) and absence (!k), joined by commas.
type Selector []term

// ParseSelector parses a label selector; the empty string matches everything.
func ParseSelector(s string) (Selector, error) {
	var out Selector
	for _, raw := range splitTop(s) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		switch {
		case strings.HasPrefix(raw, "!") && !strings.Contains(raw, "="):
			out = append(out, term{key: strings.TrimSpace(raw[1:]), op: "notexists"})
		case strings.Contains(raw, "!="):
			k, v, _ := strings.Cut(raw, "!=")
			out = append(out, term{key: strings.TrimSpace(k), op: "!=", values: []string{strings.TrimSpace(v)}})
		case strings.Contains(raw, "=="):
			k, v, _ := strings.Cut(raw, "==")
			out = append(out, term{key: strings.TrimSpace(k), op: "=", values: []string{strings.TrimSpace(v)}})
		case strings.Contains(raw, "="):
			k, v, _ := strings.Cut(raw, "=")
			out = append(out, term{key: strings.TrimSpace(k), op: "=", values: []string{strings.TrimSpace(v)}})
		case strings.Contains(raw, " notin "):
			k, v, _ := strings.Cut(raw, " notin ")
			vals, err := parseSet(v)
			if err != nil {
				return nil, err
			}
			out = append(out, term{key: strings.TrimSpace(k), op: "notin", values: vals})
		case strings.Contains(raw, " in "):
			k, v, _ := strings.Cut(raw, " in ")
			vals, err := parseSet(v)
			if err != nil {
				return nil, err
			}
			out = append(out, term{key: strings.TrimSpace(k), op: "in", values: vals})
		default:
			out = append(out, term{key: raw, op: "exists"})
		}
	}
	for _, t := range out {
		if t.key == "" {
			return nil, fmt.Errorf("label selector %q has an empty key", s)
		}
	}
	return out, nil
}

// splitTop splits at commas outside parentheses.
func splitTop(s string) []string {
	var parts []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, s[start:])
}

func parseSet(v string) ([]string, error) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "(") || !strings.HasSuffix(v, ")") {
		return nil, fmt.Errorf("label selector set %q is not (a,b)", v)
	}
	var out []string
	for e := range strings.SplitSeq(v[1:len(v)-1], ",") {
		out = append(out, strings.TrimSpace(e))
	}
	return out, nil
}

// Matches reports whether the labels satisfy every term.
func (s Selector) Matches(labels map[string]string) bool {
	for _, t := range s {
		v, ok := labels[t.key]
		switch t.op {
		case "=":
			if !ok || v != t.values[0] {
				return false
			}
		case "!=":
			if ok && v == t.values[0] {
				return false
			}
		case "in":
			if !ok || !contains(t.values, v) {
				return false
			}
		case "notin":
			if ok && contains(t.values, v) {
				return false
			}
		case "exists":
			if !ok {
				return false
			}
		case "notexists":
			if ok {
				return false
			}
		}
	}
	return true
}

func contains(l []string, v string) bool {
	return slices.Contains(l, v)
}
