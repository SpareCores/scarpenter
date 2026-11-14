/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import (
	"path/filepath"
	"testing"

	operatoroptions "github.com/SpareCores/scarpenter/pkg/operator/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func TestTheDriverIsRegistered(t *testing.T) {
	found := false
	for _, n := range vendors.Registered() {
		found = found || n == Name
	}
	if !found {
		t.Error("the oci driver is not registered")
	}
}

// The compartment and the credentials come from the controller's options when
// no Option gave them; an Option wins.
func TestApplyContextOptions(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, SecretTenancy, "ocid1.tenancy.oc1..aaaa")
	write(t, dir, SecretUser, "ocid1.user.oc1..aaaa")
	write(t, dir, SecretFingerprint, "aa:bb")
	write(t, dir, SecretRegion, "eu-frankfurt-1")
	write(t, dir, SecretKey, testKeyPEM(t))
	o := &operatoroptions.Options{OCICompartment: "ocid1.compartment.oc1..cccc", OCIImageCompartment: "ocid1.compartment.oc1..iiii", OCIAuth: AuthAPIKey, OCICredentialsDir: dir}

	d := New()
	if err := d.applyContextOptions(o.ToContext(t.Context())); err != nil {
		t.Fatal(err)
	}
	if d.Compartment() != o.OCICompartment || d.provider == nil {
		t.Errorf("compartment %q, provider %v", d.Compartment(), d.provider)
	}

	if d.ImageCompartment() != o.OCIImageCompartment {
		t.Errorf("image compartment %q", d.ImageCompartment())
	}
	if d = New(WithCompartment("c")); d.ImageCompartment() != "c" {
		t.Errorf("image compartment default %q", d.ImageCompartment())
	}

	d = New(WithCompartment("ocid1.compartment.oc1..own"))
	if err := d.applyContextOptions(o.ToContext(t.Context())); err != nil || d.Compartment() != "ocid1.compartment.oc1..own" {
		t.Errorf("an Option does not win: %q, %v", d.Compartment(), err)
	}

	bad := *o
	bad.OCICredentialsDir = filepath.Join(dir, "missing")
	if err := New().applyContextOptions(bad.ToContext(t.Context())); err == nil {
		t.Error("credentials that cannot be read were accepted")
	}
	if err := New().applyContextOptions(t.Context()); err != nil {
		t.Errorf("no options in the context: %v", err)
	}
}
