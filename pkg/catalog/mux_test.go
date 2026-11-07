/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SpareCores/scarpenter/pkg/catalog/model"
)

// recordingSource returns a fixed data set and remembers its last request.
type recordingSource struct {
	data  *model.Data
	err   error
	calls int
	last  Request
}

func (s *recordingSource) Load(_ context.Context, req Request) (*model.Data, error) {
	s.calls++
	s.last = req
	return s.data, s.err
}

func serverOf(vendor string) []model.Server {
	return []model.Server{{Vendor: vendor, ID: "s", APIReference: "s"}}
}

func TestMuxSendsEachVendorToItsSourceAndMergesTheData(t *testing.T) {
	older, newer := time.Unix(100, 0), time.Unix(200, 0)
	keeper := &recordingSource{data: &model.Data{
		Version: "k1", FetchedAt: newer, Servers: serverOf("aws"),
		Scores: map[string]map[string]float64{"m-aws": {"s": 1}}, Invalid: map[string]string{"bad-aws": "why"},
	}}
	own := &recordingSource{data: &model.Data{
		Version: "o1", FetchedAt: older, Servers: serverOf("qemu"),
		Scores: map[string]map[string]float64{"m-qemu": {"s": 2}}, Invalid: map[string]string{"m-q2": "no benchmark data"},
	}}
	mux := &MuxSource{Default: keeper, Vendors: map[string]Source{"qemu": own}}
	req := Request{
		Scope:   Scope{"aws": {"us-east-1"}, "qemu": nil},
		Metrics: []MetricQuery{{ID: "m-aws", Vendor: "aws"}, {ID: "m-qemu", Vendor: "qemu"}},
	}
	got, err := mux.Load(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := keeper.last.Scope["qemu"]; ok || len(keeper.last.Metrics) != 1 || keeper.last.Metrics[0].ID != "m-aws" {
		t.Errorf("the default source got %+v", keeper.last)
	}
	if _, ok := own.last.Scope["aws"]; ok || len(own.last.Scope) != 1 || len(own.last.Metrics) != 1 || own.last.Metrics[0].ID != "m-qemu" {
		t.Errorf("the own source got %+v", own.last)
	}
	if len(got.Servers) != 2 || got.Scores["m-aws"]["s"] != 1 || got.Scores["m-qemu"]["s"] != 2 || len(got.Invalid) != 2 {
		t.Errorf("merged data %+v", got)
	}
	if !got.FetchedAt.Equal(older) {
		t.Errorf("FetchedAt %v, want the older one", got.FetchedAt)
	}
	if got.Version == "k1" || got.Version == "o1" || got.Version == "" {
		t.Errorf("the merged version %q must depend on both parts", got.Version)
	}
	// Either part changing changes the version.
	own.data = &model.Data{Version: "o2", FetchedAt: older, Servers: serverOf("qemu")}
	again, _ := mux.Load(t.Context(), req)
	if again.Version == got.Version {
		t.Error("the version did not change with the own source's")
	}
}

func TestMuxDoesNotCallTheDefaultSourceForOnlyOwnVendors(t *testing.T) {
	keeper := &recordingSource{err: errors.New("keeper is unreachable")}
	own := &recordingSource{data: &model.Data{Version: "o1", Servers: serverOf("qemu")}}
	mux := &MuxSource{Default: keeper, Vendors: map[string]Source{"qemu": own}}
	got, err := mux.Load(t.Context(), Request{Scope: Scope{"qemu": nil}})
	if err != nil || keeper.calls != 0 || len(got.Servers) != 1 {
		t.Fatalf("%+v, %v, %d calls to the default source", got, err, keeper.calls)
	}
}

func TestMuxPropagatesAFailureOfEitherSource(t *testing.T) {
	ok := &recordingSource{data: &model.Data{Version: "v"}}
	bad := &recordingSource{err: errors.New("boom")}
	req := Request{Scope: Scope{"aws": nil, "qemu": nil}}
	for name, mux := range map[string]*MuxSource{
		"default": {Default: bad, Vendors: map[string]Source{"qemu": ok}},
		"own":     {Default: ok, Vendors: map[string]Source{"qemu": bad}},
	} {
		if _, err := mux.Load(t.Context(), req); err == nil {
			t.Errorf("%s source failing did not fail the load", name)
		}
	}
}
