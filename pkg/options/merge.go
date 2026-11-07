/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package options

import (
	"encoding/json"
	"fmt"
)

// decode parses a JSON object; nil and "null" are the empty object.
func decode(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]any{}, nil
	}
	m := map[string]any{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decoding a settings block: %w", err)
	}
	return m, nil
}

// merge overlays patch on base in place and returns base: objects merge key by
// key, everything else (scalars and lists, so the extra volumes of a profile
// replace those of the vendor block) replaces. This is JSON merge patch
// (RFC 7386) without null deletion: the blocks come from omitempty structs and
// never hold a null.
func merge(base, patch map[string]any) map[string]any {
	if base == nil {
		base = map[string]any{}
	}
	for k, v := range patch {
		pm, patchIsObject := v.(map[string]any)
		bm, baseIsObject := base[k].(map[string]any)
		if patchIsObject && baseIsObject {
			base[k] = merge(bm, pm)
			continue
		}
		base[k] = v
	}
	return base
}
