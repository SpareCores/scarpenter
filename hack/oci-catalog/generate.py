#!/usr/bin/env python3
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this
# file, You can obtain one at https://mozilla.org/MPL/2.0/.
"""Generate the embedded OCI catalog (pkg/catalog/embeddeddata/oci).

THE DATA IS A STAND-IN until Keeper serves OCI. Once it does, the catalog
reads the vendor from Keeper (remove "oci" from the embedded vendors option)
and this generator, its output and the embedded source are deleted (plan 25,
section 6, "When Keeper starts serving OCI").

Input: the SQLite database of the Spare Cores crawler (sc-crawler) for the
vendor "oci", given with --db. It is not in this repository, so no CI step
regenerates the files; rerun this script by hand when the data is refreshed:

    python3 hack/oci-catalog/generate.py --db <sc-data-oci.db> --out pkg/catalog/embeddeddata/oci

Output: the files of Keeper's snapshot layout for the vendor (gzip of columnar
JSON, {"column": [values, ...]}, values as Keeper's API serialises them), below
--out, each at "<keeper path>.json.gz":

    vendor            the one row of the vendor "oci" (billing facts)
    region/oci        every region
    zone/oci          every availability domain
    server/oci        every server row, inactive ones included
    server_price/oci  every on-demand and spot price row
    storage_price/oci the block volume prices per region
    ipv4_price/oci    the public IPv4 price per region
    manifest          path, rows and SHA-256 (of the UNCOMPRESSED JSON) of the
                      files above, the same columns as Keeper's snapshot
                      manifest

Nothing is dropped: the catalog and the driver decide (the converter skips
inactive servers). Every column of the database table is kept, with the
SQLite enum names converted to Keeper's API values: allocation ONDEMAND/SPOT
to ondemand/spot, status ACTIVE/INACTIVE to active/inactive, cpu_architecture
X86_64/ARM64 to x86_64/arm64, cpu_allocation DEDICATED to Dedicated (likewise
Shared, Burstable), unit HOUR/MONTH/GB_MONTH to hour/month/GB/month, storage
type in lower case; JSON text columns are parsed, booleans are true/false and
timestamps are ISO 8601. The prices are as of the data's observed_at columns;
no other time enters the output.

The output is reproducible: rows are sorted by primary key, keys have a fixed
order, floats are Python's shortest repr, and the gzip header has a zero mtime
and no file name. The SHA-256 in the manifest is of the uncompressed JSON, so
it is stable whatever zlib built the gzip bytes. The script prints the counts
and every row it could not convert (a value outside the maps above); such rows
are left out and the exit status is 1.
"""

import argparse
import gzip
import hashlib
import json
import os
import sqlite3
import sys
import urllib.parse

VENDOR = "oci"

STATUS = {"ACTIVE": "active", "INACTIVE": "inactive",
          "PLANNED_FOR_RETIREMENT": "planned-for-retirement"}
ALLOCATION = {"ONDEMAND": "ondemand", "SPOT": "spot", "RESERVED": "reserved"}
ARCHITECTURE = {"X86_64": "x86_64", "ARM64": "arm64", "I386": "i386"}
CPU_ALLOCATION = {"DEDICATED": "Dedicated", "SHARED": "Shared", "BURSTABLE": "Burstable"}
UNIT = {"HOUR": "hour", "MONTH": "month", "GB_MONTH": "GB/month"}

# Per column: a map (the value must be a key) or a function.
BOOLEAN = "bool"
JSON = "json"
LOWER = "lower"
TIMESTAMP = "ts"

COLUMN_RULES = {
    "status": STATUS,
    "allocation": ALLOCATION,
    "cpu_architecture": ARCHITECTURE,
    "cpu_allocation": CPU_ALLOCATION,
    "unit": UNIT,
    "storage_type": LOWER,
    "observed_at": TIMESTAMP,
    "hw_virt": BOOLEAN,
    "memory_ecc": BOOLEAN,
    "green_energy": BOOLEAN,
    "stopped_server_charged": BOOLEAN,
    "api_reference_object": JSON,
    "aliases": JSON,
    "cpu_flags": JSON,
    "cpus": JSON,
    "accelerators": JSON,
    "storages": JSON,
    "compatible_storage_ids": JSON,
    "price_tiered": JSON,
}

# table, Keeper path, WHERE, ORDER BY (the primary key).
TABLES = [
    ("vendor", "vendor", "vendor_id = :v", "vendor_id"),
    ("region", "region/" + VENDOR, "vendor_id = :v", "region_id"),
    ("zone", "zone/" + VENDOR, "vendor_id = :v", "region_id, zone_id"),
    ("server", "server/" + VENDOR, "vendor_id = :v", "server_id"),
    ("server_price", "server_price/" + VENDOR, "vendor_id = :v",
     "region_id, zone_id, server_id, allocation, operating_system"),
    ("storage_price", "storage_price/" + VENDOR, "vendor_id = :v", "region_id, storage_id"),
    ("ipv4_price", "ipv4_price/" + VENDOR, "vendor_id = :v", "region_id"),
]


class ConvertError(Exception):
    pass


def convert(column, value):
    """Convert one SQLite value to the value of Keeper's API."""
    rule = COLUMN_RULES.get(column)
    if value is None or rule is None:
        return value
    if rule == BOOLEAN:
        return bool(value)
    if rule == JSON:
        return json.loads(value) if isinstance(value, str) else value
    if rule == LOWER:
        return value.lower()
    if rule == TIMESTAMP:
        return value.replace(" ", "T")
    if value not in rule:
        raise ConvertError("%s=%r" % (column, value))
    return rule[value]


def dump(columns, rows):
    """Columnar JSON of the rows, as bytes."""
    doc = {c: [r[c] for r in rows] for c in columns}
    return json.dumps(doc, separators=(",", ":"), ensure_ascii=False,
                      allow_nan=False).encode("utf-8")


def write(out, path, data):
    file = os.path.join(out, *path.split("/")) + ".json.gz"
    os.makedirs(os.path.dirname(file), exist_ok=True)
    with open(file, "wb") as f:
        f.write(gzip.compress(data, compresslevel=9, mtime=0))


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--db", required=True, help="the crawler's SQLite database")
    ap.add_argument("--out", required=True, help="the output directory")
    args = ap.parse_args()

    uri = "file:" + urllib.parse.quote(os.path.abspath(args.db)) + "?mode=ro"
    db = sqlite3.connect(uri, uri=True)
    db.row_factory = sqlite3.Row

    manifest = []
    failures = 0
    for table, path, where, order in TABLES:
        cur = db.execute("SELECT * FROM %s WHERE %s ORDER BY %s" % (table, where, order),
                         {"v": VENDOR})
        columns = [d[0] for d in cur.description]
        rows = []
        for raw in cur:
            try:
                rows.append({c: convert(c, raw[c]) for c in columns})
            except ConvertError as e:
                failures += 1
                print("cannot convert %s row %s: %s" % (
                    table, [raw[k] for k in raw.keys() if k.endswith("_id")], e),
                    file=sys.stderr)
        data = dump(columns, rows)
        write(args.out, path, data)
        manifest.append({"path": path, "rows": len(rows),
                         "sha256": hashlib.sha256(data).hexdigest()})
        print("%-20s %6d rows" % (path, len(rows)))

    manifest.sort(key=lambda m: m["path"])
    write(args.out, "manifest",
          dump(["path", "rows", "sha256"], manifest))
    print("%-20s %6d files" % ("manifest", len(manifest)))
    if failures:
        print("%d rows could not be converted and were left out" % failures, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
