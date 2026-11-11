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

// Keywords and type names of the schema, as the conversion and the validator use them.
const (
	kwType        = "type"
	kwDescription = "description"
	kwAdditional  = "additionalProperties"
	kwAllOf       = "allOf"
	kwAnyOf       = "anyOf"
	kwOneOf       = "oneOf"
	typeObject    = "object"
	typeString    = "string"
	typeInteger   = "integer"
	typeNumber    = "number"
)

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
	props["apiVersion"] = map[string]any{"const": group + "/" + version, kwDescription: "The API group and version."}
	props["kind"] = map[string]any{"const": kind, kwDescription: "The kind of the object."}
	// A cluster-scoped object is named; its other metadata is the API server's.
	props["metadata"] = map[string]any{
		kwType: typeObject, kwDescription: "Standard object metadata; only the name is required.",
		"properties": map[string]any{
			"name":        map[string]any{kwType: typeString, "minLength": 1, "maxLength": 253},
			"labels":      map[string]any{kwType: typeObject, kwAdditional: map[string]any{kwType: typeString}},
			"annotations": map[string]any{kwType: typeObject, kwAdditional: map[string]any{kwType: typeString}},
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
		case kwAdditional:
			if m, ok := v.(map[string]any); ok {
				out[k] = convert(m, "")
			} else {
				out[k] = v
			}
		case kwAllOf, kwAnyOf, kwOneOf:
			l := make([]any, 0, len(asSlice(v)))
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
		delete(out, kwType)
		out[kwAnyOf] = []any{map[string]any{kwType: typeInteger}, map[string]any{kwType: typeString}}
	}
	if in["nullable"] == true {
		if t, ok := out[kwType].(string); ok {
			out[kwType] = []any{t, "null"}
		}
	}
	if in["x-kubernetes-list-type"] == "set" || in["x-kubernetes-list-type"] == "map" {
		out["uniqueItems"] = true
	}
	// A structural schema prunes the fields it does not list: reject them, unless
	// the object holds free-form content or its values are a typed map.
	if _, has := out["properties"]; has && !preserve && out[kwAdditional] == nil && name != "metadata" {
		out[kwAdditional] = false
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
		return []string{typeString}
	case []any:
		return []string{"array"}
	case map[string]any:
		return []string{typeObject}
	case float64:
		if x == float64(int64(x)) {
			return []string{typeInteger, typeNumber}
		}
		return []string{typeNumber}
	case int, int32, int64:
		return []string{typeInteger, typeNumber}
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

// check is the state of the validation of one value.
type check struct {
	s    map[string]any
	path string
	errs *[]error
}

func (c check) fail(format string, args ...any) {
	*c.errs = append(*c.errs, fmt.Errorf("%s: "+format, append([]any{c.path}, args...)...))
}

func validate(schema, doc any, path string, errs *[]error) {
	s, ok := schema.(map[string]any)
	if !ok {
		if schema == false {
			*errs = append(*errs, fmt.Errorf("%s: not allowed", path))
		}
		return
	}
	c := check{s: s, path: path, errs: errs}
	if !c.typeOK(doc) {
		return
	}
	if k, ok := s["const"]; ok && fmt.Sprint(k) != fmt.Sprint(doc) {
		c.fail("is %v, want %v", doc, k)
	}
	if e, ok := s["enum"]; ok &&
		!slices.ContainsFunc(asSlice(e), func(x any) bool { return fmt.Sprint(x) == fmt.Sprint(doc) }) {
		c.fail("is %v, want one of %v", doc, e)
	}
	c.combinators(doc)
	switch x := doc.(type) {
	case string:
		c.str(x)
	case float64, int, int32, int64:
		f, _ := num(x)
		c.number(f)
	case []any:
		c.array(x)
	case map[string]any:
		c.object(x)
	}
}

// typeOK checks the type keyword; false means the value is of another type and nothing
// else is checked.
func (c check) typeOK(doc any) bool {
	t, ok := c.s[kwType]
	if !ok {
		return true
	}
	var want []string
	switch x := t.(type) {
	case string:
		want = []string{x}
	case []any:
		for _, e := range x {
			want = append(want, e.(string))
		}
	}
	if slices.ContainsFunc(typeOf(doc), func(have string) bool { return slices.Contains(want, have) }) {
		return true
	}
	c.fail("is %s, want %s", typeOf(doc)[0], strings.Join(want, " or "))
	return false
}

func (c check) combinators(doc any) {
	for _, kw := range []string{kwAnyOf, kwOneOf, kwAllOf} {
		subs := asSlice(c.s[kw])
		if len(subs) == 0 {
			continue
		}
		matches := 0
		for _, sub := range subs {
			var sub1 []error
			validate(sub, doc, c.path, &sub1)
			if len(sub1) == 0 {
				matches++
			}
		}
		if (kw == kwAllOf && matches != len(subs)) || (kw == kwAnyOf && matches == 0) || (kw == kwOneOf && matches != 1) {
			c.fail("does not satisfy %s", kw)
		}
	}
}

func (c check) str(x string) {
	if n, ok := num(c.s["minLength"]); ok && float64(len([]rune(x))) < n {
		c.fail("is shorter than %v characters", n)
	}
	if n, ok := num(c.s["maxLength"]); ok && float64(len([]rune(x))) > n {
		c.fail("is longer than %v characters", n)
	}
	if p, ok := c.s["pattern"].(string); ok {
		if re, err := regexp.Compile(p); err == nil && !re.MatchString(x) {
			c.fail("does not match %s", p)
		}
	}
}

func (c check) number(f float64) {
	if n, ok := num(c.s["minimum"]); ok && f < n {
		c.fail("is below %v", n)
	}
	if n, ok := num(c.s["maximum"]); ok && f > n {
		c.fail("is above %v", n)
	}
}

func (c check) array(x []any) {
	if n, ok := num(c.s["minItems"]); ok && float64(len(x)) < n {
		c.fail("has fewer than %v items", n)
	}
	if n, ok := num(c.s["maxItems"]); ok && float64(len(x)) > n {
		c.fail("has more than %v items", n)
	}
	if c.s["uniqueItems"] == true {
		seen := map[string]bool{}
		for _, e := range x {
			b, _ := json.Marshal(e)
			if seen[string(b)] {
				c.fail("lists %s twice", b)
			}
			seen[string(b)] = true
		}
	}
	if items, ok := c.s["items"]; ok {
		for i, e := range x {
			validate(items, e, fmt.Sprintf("%s[%d]", c.path, i), c.errs)
		}
	}
}

func (c check) object(x map[string]any) {
	if n, ok := num(c.s["minProperties"]); ok && float64(len(x)) < n {
		c.fail("has fewer than %v properties", n)
	}
	if n, ok := num(c.s["maxProperties"]); ok && float64(len(x)) > n {
		c.fail("has more than %v properties", n)
	}
	for _, r := range asSlice(c.s["required"]) {
		if _, ok := x[r.(string)]; !ok {
			c.fail("lacks the required field %q", r)
		}
	}
	props, _ := c.s["properties"].(map[string]any)
	for _, k := range slices.Sorted(maps.Keys(x)) {
		sub, known := props[k]
		switch {
		case known:
			validate(sub, x[k], c.path+"."+k, c.errs)
		case c.s[kwAdditional] == false:
			c.fail("has the unknown field %q", k)
		default:
			if ap, ok := c.s[kwAdditional].(map[string]any); ok {
				validate(ap, x[k], c.path+"."+k, c.errs)
			}
		}
	}
}
