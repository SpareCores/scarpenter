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

// The metadata restriction reaches the render request as the fragment the core builds
// from the driver's declaration: on by default and with Restricted, off with Open.
func TestCreateGivesTheRendererTheMetadataFragment(t *testing.T) {
	access := func(a v1alpha1.InstanceMetadataAccess) func(*v1alpha1.SCNodeClass) {
		return func(nc *v1alpha1.SCNodeClass) { nc.Spec.InstanceMetadata = &v1alpha1.InstanceMetadataSpec{Access: a} }
	}
	for name, tc := range map[string]struct {
		mutate func(*v1alpha1.SCNodeClass)
		want   []string
	}{
		"default":    {nil, []string{"metadata-access"}},
		"restricted": {access(v1alpha1.InstanceMetadataRestricted), []string{"metadata-access"}},
		"hostonly":   {access(v1alpha1.InstanceMetadataHostOnly), []string{"metadata-access"}},
		"open":       {access(v1alpha1.InstanceMetadataOpen), nil},
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
				if !strings.Contains(f.Butane, "169.254.169.254") || !strings.Contains(f.Butane, "fake-agent.service") {
					t.Errorf("fragment %s: %s", f.Name, f.Butane)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("fragments = %v, want %v", got, tc.want)
			}
		})
	}
}
