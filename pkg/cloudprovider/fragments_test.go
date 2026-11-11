/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
)

// The hardening of the NodeClass reaches the render request as the fragments of the
// driver; without it the request has none.
func TestCreateGivesTheRendererTheDriversFragments(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*v1alpha1.SCNodeClass)
		want   []string
	}{
		"off": {nil, nil},
		"on": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Bootstrap = &v1alpha1.BootstrapSpec{Hardening: []v1alpha1.Hardening{v1alpha1.HardeningMetadataFromPods}}
		}, []string{"block-metadata-from-pods"}},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, envOptions{transform: tc.mutate})
			if _, err := e.cp.Create(t.Context(), e.newClaim("claim-frag-"+name, []string{"aws.m7i.large"})); err != nil {
				t.Fatal(err)
			}
			frags := e.renderer.last().VendorFragments
			got := make([]string, 0, len(frags))
			for _, f := range frags {
				got = append(got, f.Name)
				if !strings.Contains(f.Butane, "169.254.169.254") {
					t.Errorf("fragment %s: %s", f.Name, f.Butane)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("fragments = %v, want %v", got, tc.want)
			}
		})
	}
}
