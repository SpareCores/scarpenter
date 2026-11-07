/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vendors

// CatalogProvider is the optional capability of a development driver whose
// instance types are not in Keeper: it returns its catalog as the YAML document
// of pkg/catalog/static. The operator serves the driver's vendor from that
// document and every other vendor from Keeper.
type CatalogProvider interface {
	CatalogYAML() []byte
}
