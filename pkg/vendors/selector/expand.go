/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package selector is the shared half of the `use` selector (ADR-023, plan 14
// A10): placeholder expansion, glob and natural-order comparison, tag, CIDR and
// attribute matching, and the selection of one or several matches with typed
// errors that carry the count after each stage. It imports no vendor SDK: a
// driver supplies the vendor call and an accessor per item, and never writes its
// own matching.
package selector

import (
	"fmt"
	"slices"
	"strings"
)

// The names of the placeholders.
const (
	phRegion     = "region"
	phZone       = "zone"
	phArch       = "arch"
	phVendorArch = "vendorArch"
)

// Placeholders are the names that may appear in braces in a string of `use`.
var Placeholders = []string{phRegion, phZone, phArch, phVendorArch, "channel", "version", "nodeclass"}

// Context is the value of each placeholder. An empty value means that the
// placeholder has none in this place: using it is an error.
type Context struct {
	Region    string
	Zone      string
	Arch      string
	Channel   string
	Version   string
	NodeClass string
	// VendorArch translates Arch to the vendor's own spelling; nil when the
	// vendor has none.
	VendorArch func(arch string) (string, bool)
}

func (c Context) value(name string) (string, error) {
	var v string
	switch name {
	case phRegion:
		v = c.Region
	case phZone:
		v = c.Zone
	case phArch:
		v = c.Arch
	case phVendorArch:
		if c.Arch != "" && c.VendorArch != nil {
			v, _ = c.VendorArch(c.Arch)
		}
	case "channel":
		v = c.Channel
	case "version":
		v = c.Version
	case "nodeclass":
		v = c.NodeClass
	default:
		return "", fmt.Errorf("unknown placeholder {%s} (known: %s)", name, strings.Join(Placeholders, ", "))
	}
	if v == "" {
		return "", fmt.Errorf("the placeholder {%s} has no value here", name)
	}
	return v, nil
}

// scan walks s, calling lit for literal text and ph for each placeholder name.
// {{ and }} are a literal brace; any other brace is an error.
func scan(s string, lit func(string), ph func(string) error) error {
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			lit(b.String())
			b.Reset()
		}
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '{':
			if i+1 < len(s) && s[i+1] == '{' {
				b.WriteByte('{')
				i++
				continue
			}
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				return fmt.Errorf("%q: a { without a closing } (write {{ for a literal brace)", s)
			}
			name := s[i+1 : i+end]
			if name == "" || strings.ContainsAny(name, "{ ") {
				return fmt.Errorf("%q: %q is not a placeholder (write {{ and }} for literal braces)", s, s[i:i+end+1])
			}
			flush()
			if err := ph(name); err != nil {
				return fmt.Errorf("%q: %w", s, err)
			}
			i += end
		case '}':
			if i+1 < len(s) && s[i+1] == '}' {
				b.WriteByte('}')
				i++
				continue
			}
			return fmt.Errorf("%q: a } without an opening { (write }} for a literal brace)", s)
		default:
			b.WriteByte(c)
		}
	}
	flush()
	return nil
}

// Expand replaces the placeholders in s with the context's values. {{ and }}
// are a literal brace.
func Expand(s string, c Context) (string, error) {
	var out strings.Builder
	err := scan(s, func(l string) { out.WriteString(l) }, func(name string) error {
		v, err := c.value(name)
		if err != nil {
			return err
		}
		out.WriteString(v)
		return nil
	})
	if err != nil {
		return "", err
	}
	return out.String(), nil
}

// Placeholdered returns the placeholder names s uses, and the first syntax
// error.
func Placeholdered(s string) (names []string, err error) {
	err = scan(s, func(string) {}, func(name string) error {
		if slices.Contains(Placeholders, name) {
			names = append(names, name)
			return nil
		}
		return fmt.Errorf("unknown placeholder {%s} (known: %s)", name, strings.Join(Placeholders, ", "))
	})
	return names, err
}
