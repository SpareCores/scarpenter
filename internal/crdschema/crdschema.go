/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package crdschema turns the generated SCNodeClass CRD into a standalone JSON
// Schema (draft 2020-12) that an infrastructure tool can validate the NodeClass it
// renders against without a cluster or any Go (docs/reference/infrastructure-handoff.md,
// "Published schema and examples"), and validates a document against it with the
// subset of JSON Schema that conversion produces.
//
// The conversion keeps what JSON Schema can say (types, required, enums, bounds,
// patterns, formats, defaults, descriptions) and drops what it cannot: the CEL rules
// (x-kubernetes-validations) and the list and map hints. Unknown fields are
// rejected, as a structural schema prunes them, so a misspelt field is an error.
// The CEL rules are enforced only by the API server.
package crdschema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
)

// Draft is the JSON Schema dialect of the output.
const Draft = "https://json-schema.org/draft/2020-12/schema"

// Convert returns the JSON Schema of the first version of the CRD, as
// indented JSON with sorted keys (deterministic: the output is checked in).
func Convert(crdYAML []byte) ([]byte, error) {
	var crd map[string]any
	if err := yaml.Unmarshal(crdYAML, &crd); err != nil {
		return nil, fmt.Errorf("parsing the CRD: %w", err)
	}
	spec, _ := crd["spec"].(map[string]any)
	versions, _ := spec["versions"].([]any)
	if len(versions) == 0 {
		return nil, errors.New("the CRD has no versions")
	}
	v, _ := versions[0].(map[string]any)
	root, _ := dig(v, "schema", "openAPIV3Schema").(map[string]any)
	if root == nil {
		return nil, errors.New("the CRD has no openAPIV3Schema")
	}
	group, _ := spec["group"].(string)
	kind, _ := dig(spec, "names", "kind").(string)
	version, _ := v["name"].(string)

	out := convert(root, "")
	out["$schema"] = Draft
	out["title"] = fmt.Sprintf("%s (%s/%s)", kind, group, version)
	props := out["properties"].(map[string]any)
	props["apiVersion"] = map[string]any{"const": group + "/" + version, "description": "The API group and version."}
	props["kind"] = map[string]any{"const": kind, "description": "The kind of the object."}
	// A cluster-scoped object is named; its other metadata is the API server's.
	props["metadata"] = map[string]any{
		"type": "object", "description": "Standard object metadata; only the name is required.",
		"properties": map[string]any{
			"name":        map[string]any{"type": "string", "minLength": 1, "maxLength": 253},
			"labels":      map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
			"annotations": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
		},
		"required": []any{"name"},
	}
	req := []any{"apiVersion", "kind", "metadata"}
	for _, r := range asSlice(root["required"]) {
		if !slices.Contains(req, r) {
			req = append(req, r)
		}
	}
	out["required"] = req

	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func dig(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// convert converts one OpenAPI v3 schema of a CRD to JSON Schema. name is the
// property it is the value of, "" for the root.
func convert(in map[string]any, name string) map[string]any {
	out := map[string]any{}
	preserve := in["x-kubernetes-preserve-unknown-fields"] == true
	for k, v := range in {
		switch k {
		case "properties":
			props := map[string]any{}
			for pn, pv := range v.(map[string]any) {
				props[pn] = convert(pv.(map[string]any), pn)
			}
			out[k] = props
		case "items":
			out[k] = convert(v.(map[string]any), "")
		case "additionalProperties":
			if m, ok := v.(map[string]any); ok {
				out[k] = convert(m, "")
			} else {
				out[k] = v
			}
		case "allOf", "anyOf", "oneOf":
			var l []any
			for _, s := range asSlice(v) {
				l = append(l, convert(s.(map[string]any), ""))
			}
			out[k] = l
		case "nullable", "x-kubernetes-validations", "x-kubernetes-list-type", "x-kubernetes-list-map-keys",
			"x-kubernetes-map-type", "x-kubernetes-preserve-unknown-fields", "x-kubernetes-embedded-resource",
			"x-kubernetes-int-or-string":
			// handled below or not expressible
		default:
			out[k] = v
		}
	}
	if in["x-kubernetes-int-or-string"] == true {
		delete(out, "type")
		out["anyOf"] = []any{map[string]any{"type": "integer"}, map[string]any{"type": "string"}}
	}
	if in["nullable"] == true {
		if t, ok := out["type"].(string); ok {
			out["type"] = []any{t, "null"}
		}
	}
	if in["x-kubernetes-list-type"] == "set" || in["x-kubernetes-list-type"] == "map" {
		out["uniqueItems"] = true
	}
	// A structural schema prunes the fields it does not list: reject them, unless
	// the object holds free-form content or its values are a typed map.
	if _, has := out["properties"]; has && !preserve && out["additionalProperties"] == nil && name != "metadata" {
		out["additionalProperties"] = false
	}
	return out
}

// Validate checks doc (decoded JSON or YAML) against the schema (decoded JSON) with the
// keywords Convert produces: type, const, enum, properties, required,
// additionalProperties, items, anyOf, allOf and oneOf, the bounds on numbers,
// strings, arrays and objects, uniqueItems and pattern. It returns one error per
// violation, with the path of the value; format and default are annotations.
func Validate(schema, doc any) []error {
	var errs []error
	validate(schema, doc, "$", &errs)
	return errs
}

func typeOf(v any) []string {
	switch x := v.(type) {
	case nil:
		return []string{"null"}
	case bool:
		return []string{"boolean"}
	case string:
		return []string{"string"}
	case []any:
		return []string{"array"}
	case map[string]any:
		return []string{"object"}
	case float64:
		if x == float64(int64(x)) {
			return []string{"integer", "number"}
		}
		return []string{"number"}
	case int, int32, int64:
		return []string{"integer", "number"}
	}
	return []string{fmt.Sprintf("%T", v)}
}

func num(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

func validate(schema, doc any, path string, errs *[]error) {
	s, ok := schema.(map[string]any)
	if !ok {
		if schema == false {
			*errs = append(*errs, fmt.Errorf("%s: not allowed", path))
		}
		return
	}
	fail := func(format string, args ...any) {
		*errs = append(*errs, fmt.Errorf("%s: "+format, append([]any{path}, args...)...))
	}

	if t, ok := s["type"]; ok {
		var want []string
		switch x := t.(type) {
		case string:
			want = []string{x}
		case []any:
			for _, e := range x {
				want = append(want, e.(string))
			}
		}
		if !slices.ContainsFunc(typeOf(doc), func(have string) bool { return slices.Contains(want, have) }) {
			fail("is %s, want %s", typeOf(doc)[0], strings.Join(want, " or "))
			return
		}
	}
	if c, ok := s["const"]; ok && fmt.Sprint(c) != fmt.Sprint(doc) {
		fail("is %v, want %v", doc, c)
	}
	if e, ok := s["enum"]; ok {
		if !slices.ContainsFunc(asSlice(e), func(x any) bool { return fmt.Sprint(x) == fmt.Sprint(doc) }) {
			fail("is %v, want one of %v", doc, e)
		}
	}
	for _, kw := range []string{"anyOf", "oneOf", "allOf"} {
		subs := asSlice(s[kw])
		if len(subs) == 0 {
			continue
		}
		matches := 0
		for _, sub := range subs {
			var sub1 []error
			validate(sub, doc, path, &sub1)
			if len(sub1) == 0 {
				matches++
			}
		}
		switch {
		case kw == "allOf" && matches != len(subs), kw == "anyOf" && matches == 0, kw == "oneOf" && matches != 1:
			fail("does not satisfy %s", kw)
		}
	}
	switch x := doc.(type) {
	case string:
		if n, ok := num(s["minLength"]); ok && float64(len([]rune(x))) < n {
			fail("is shorter than %v characters", n)
		}
		if n, ok := num(s["maxLength"]); ok && float64(len([]rune(x))) > n {
			fail("is longer than %v characters", n)
		}
		if p, ok := s["pattern"].(string); ok {
			if re, err := regexp.Compile(p); err == nil && !re.MatchString(x) {
				fail("does not match %s", p)
			}
		}
	case float64, int, int32, int64:
		f, _ := num(x)
		if n, ok := num(s["minimum"]); ok && f < n {
			fail("is below %v", n)
		}
		if n, ok := num(s["maximum"]); ok && f > n {
			fail("is above %v", n)
		}
	case []any:
		if n, ok := num(s["minItems"]); ok && float64(len(x)) < n {
			fail("has fewer than %v items", n)
		}
		if n, ok := num(s["maxItems"]); ok && float64(len(x)) > n {
			fail("has more than %v items", n)
		}
		if s["uniqueItems"] == true {
			seen := map[string]bool{}
			for _, e := range x {
				b, _ := json.Marshal(e)
				if seen[string(b)] {
					fail("lists %s twice", b)
				}
				seen[string(b)] = true
			}
		}
		if items, ok := s["items"]; ok {
			for i, e := range x {
				validate(items, e, fmt.Sprintf("%s[%d]", path, i), errs)
			}
		}
	case map[string]any:
		if n, ok := num(s["minProperties"]); ok && float64(len(x)) < n {
			fail("has fewer than %v properties", n)
		}
		if n, ok := num(s["maxProperties"]); ok && float64(len(x)) > n {
			fail("has more than %v properties", n)
		}
		for _, r := range asSlice(s["required"]) {
			if _, ok := x[r.(string)]; !ok {
				fail("lacks the required field %q", r)
			}
		}
		props, _ := s["properties"].(map[string]any)
		for _, k := range slices.Sorted(maps.Keys(x)) {
			sub, known := props[k]
			switch {
			case known:
				validate(sub, x[k], path+"."+k, errs)
			case s["additionalProperties"] == false:
				fail("has the unknown field %q", k)
			default:
				if ap, ok := s["additionalProperties"].(map[string]any); ok {
					validate(ap, x[k], path+"."+k, errs)
				}
			}
		}
	}
}
