/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vendors_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func TestInstanceRef(t *testing.T) {
	ref := vendors.NewInstanceRef("us-east-1", "i-0abc/x")
	if ref.Region() != "us-east-1" || ref.ID() != "i-0abc/x" || ref.String() != "us-east-1/i-0abc/x" {
		t.Errorf("ref %q: region %q id %q", ref, ref.Region(), ref.ID())
	}
}

func TestErrorClasses(t *testing.T) {
	base := errors.New("boom")
	wrapped := fmt.Errorf("creating: %w", vendors.NewError(vendors.QuotaExceeded, "VcpuLimitExceeded", base))
	if vendors.ClassOf(wrapped) != vendors.QuotaExceeded || !vendors.IsClass(wrapped, vendors.QuotaExceeded) ||
		vendors.IsClass(wrapped, vendors.InsufficientCapacity) {
		t.Errorf("class of %v is %q", wrapped, vendors.ClassOf(wrapped))
	}
	if vendors.CodeOf(wrapped) != "VcpuLimitExceeded" || !errors.Is(wrapped, base) {
		t.Errorf("code %q, unwrap %v", vendors.CodeOf(wrapped), errors.Is(wrapped, base))
	}
	if vendors.ClassOf(base) != vendors.Fatal {
		t.Error("an unclassified error must be Fatal")
	}
	if vendors.ClassOf(nil) != "" || vendors.IsClass(nil, vendors.Fatal) {
		t.Error("nil has no class")
	}
	for _, c := range vendors.Classes {
		if !c.Valid() {
			t.Errorf("%s is not valid", c)
		}
	}
	if vendors.Class("Nope").Valid() {
		t.Error("an unknown class is valid")
	}
}

type stub struct {
	vendors.Driver
	vendor string
	err    error
}

func (s *stub) Vendor() string                                 { return s.vendor }
func (s *stub) Init(_ context.Context, _ vendors.Config) error { return s.err }

func TestRegistry(t *testing.T) {
	vendors.Register("stub-a", func() vendors.Driver { return &stub{vendor: "alpha"} })
	vendors.Register("stub-b", func() vendors.Driver { return &stub{vendor: "alpha"} })
	vendors.Register("stub-bad", func() vendors.Driver { return &stub{vendor: "bad", err: errors.New("no credentials")} })
	defer func() {
		if recover() == nil {
			t.Error("a duplicate registration did not panic")
		}
	}()

	reg, err := vendors.NewRegistry(t.Context(), []string{"stub-a"}, vendors.Config{})
	if err != nil || !reg.Enabled("alpha") || reg.Enabled("stub-a") || len(reg.Vendors()) != 1 {
		t.Fatalf("registry: %v, %v", reg, err)
	}
	if _, ok := reg.Driver("alpha"); !ok {
		t.Error("driver not found by vendor ID")
	}
	for name, enabled := range map[string][]string{
		"unknown":   {"nope"},
		"two ids":   {"stub-a", "stub-b"},
		"init fail": {"stub-bad"},
	} {
		if _, err := vendors.NewRegistry(t.Context(), enabled, vendors.Config{}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	vendors.Register("stub-a", nil)
}

// stubFinder answers FindInstance from a fixed instance.
type stubFinder struct {
	vendors.Driver
	inst *vendors.Instance
	err  error
	got  map[vendors.TagKey]string
}

func (s *stubFinder) FindInstance(_ context.Context, _ string, tags map[vendors.TagKey]string) (*vendors.Instance, error) {
	s.got = tags
	return s.inst, s.err
}

func TestIdempotencyTag(t *testing.T) {
	in := map[vendors.TagKey]string{vendors.TagCluster: "c"}
	out := vendors.IdempotencyTag(in, "0b6c3f2e-0000-4000-8000-000000000001")
	if _, ok := in[vendors.TagIdempotencyKey]; ok {
		t.Error("the input map was modified")
	}
	if out[vendors.TagCluster] != "c" || vendors.IdempotencyKeyOf(out) != "0b6c3f2e-0000-4000-8000-000000000001" {
		t.Errorf("tags %v", out)
	}
	if _, ok := vendors.IdempotencyTag(in, "")[vendors.TagIdempotencyKey]; ok {
		t.Error("an empty key must add no tag")
	}
	if vendors.DefaultTagKey(vendors.TagIdempotencyKey) != "scarpenter.sparecores.com/idempotency-key" {
		t.Errorf("default tag key %q", vendors.DefaultTagKey(vendors.TagIdempotencyKey))
	}
	if !slices.Contains(vendors.TagKeys, vendors.TagIdempotencyKey) {
		t.Error("idempotency-key is not a logical tag key")
	}
}

func TestFindByIdempotencyKey(t *testing.T) {
	running := &vendors.Instance{State: vendors.StateRunning}
	for _, tc := range []struct {
		name string
		stub *stubFinder
		key  string
		want *vendors.Instance
		err  bool
	}{
		{"found", &stubFinder{inst: running}, "k", running, false},
		{"unused key", &stubFinder{err: vendors.Errorf(vendors.NotFound, "none")}, "k", nil, false},
		{"terminated", &stubFinder{inst: &vendors.Instance{State: vendors.StateTerminated}}, "k", nil, false},
		{"empty key", &stubFinder{inst: running}, "", nil, false},
		{"vendor error", &stubFinder{err: vendors.Errorf(vendors.Retryable, "throttled")}, "k", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := vendors.FindByIdempotencyKey(context.Background(), tc.stub, "r", tc.key)
			if got != tc.want || (err != nil) != tc.err {
				t.Errorf("got %v, %v; want %v, error %v", got, err, tc.want, tc.err)
			}
			if tc.key != "" && tc.stub.got[vendors.TagIdempotencyKey] != tc.key {
				t.Errorf("looked up %v, want the key tag only", tc.stub.got)
			}
		})
	}
}
