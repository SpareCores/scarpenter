/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package oci holds the embedded catalog of the vendor oci: files of Keeper's
// snapshot layout, written by hack/oci-catalog/generate.py (see its
// documentation for the format and for how to regenerate them).
//
// The data is a stand-in until Keeper serves OCI: it is static, and goes away
// with the generator when the catalog reads the vendor from Keeper.
package oci

import "embed"

// FS has one gzip file of columnar JSON per Keeper snapshot path, at
// "<path>.json.gz" ("server/oci.json.gz", "vendor.json.gz"), and
// "manifest.json.gz" listing them with their row counts and the SHA-256 of
// the uncompressed JSON.
//
//go:embed manifest.json.gz vendor.json.gz region zone server server_price storage_price ipv4_price
var FS embed.FS
