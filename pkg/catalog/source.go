/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	selapi "github.com/SpareCores/sc-keeper-go/selection/api"

	"github.com/SpareCores/scarpenter/pkg/catalog/model"
)

// Source provides the data a catalog is built from: Keeper for production
// (KeeperSource), a static file for development drivers (static.Source).
type Source interface {
	// Load reads one consistent version of the data for the request.
	Load(ctx context.Context, req Request) (*model.Data, error)
}

// Request says what a refresh reads.
type Request struct {
	// Scope is the vendors and regions to read prices and locations for.
	Scope Scope
	// Metrics asks for benchmark scores, per vendor.
	Metrics []MetricQuery
	// Selections asks for the members of selection profiles.
	Selections []SelectionQuery
}

// MetricQuery asks for the scores of one benchmark (or workload profile) of a
// vendor's types. Scores do not depend on the region, so the whole vendor is
// asked for.
type MetricQuery struct {
	// ID is chosen by the caller (see MetricQueryID) and keys Data.Scores.
	ID     string
	Vendor string
	// Exactly one of Benchmark and WorkloadProfile is set.
	Benchmark       *selapi.BenchmarkRef
	WorkloadProfile selapi.WorkloadProfileName
}

// SelectionQuery asks for the members of one selection profile among a vendor's
// types, evaluated over the regions the NodeClass allows (empty: all).
type SelectionQuery struct {
	// ID is chosen by the caller (see SelectionQueryID) and keys Data.Selected.
	ID      string
	Vendor  string
	Regions []string
	Spec    selapi.Spec
}

// MetricQueryID is the stable ID of a metric query: equal definitions of
// different NodeClasses share one query.
func MetricQueryID(vendor string, benchmark *selapi.BenchmarkRef, profile selapi.WorkloadProfileName) string {
	return hashID("metric", struct {
		Vendor    string
		Benchmark *selapi.BenchmarkRef
		Profile   selapi.WorkloadProfileName
	}{vendor, benchmark, profile})
}

// SelectionQueryID is the stable ID of a selection query.
func SelectionQueryID(vendor string, regions []string, spec *selapi.Spec) string {
	return hashID("selection", struct {
		Vendor  string
		Regions []string
		Spec    *selapi.Spec
	}{vendor, regions, spec})
}

func hashID(kind string, v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		// The inputs are plain API values; a failure is a programming error.
		panic(fmt.Sprintf("hashing %s query: %v", kind, err))
	}
	sum := sha256.Sum256(raw)
	return kind + "-" + hex.EncodeToString(sum[:])[:16]
}
