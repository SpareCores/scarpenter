/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func golden(t *testing.T, name string, v any) {
	t.Helper()
	got, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", name)
	if os.Getenv("UPDATE") != "" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs (UPDATE=1 rewrites it):\n%s", name, got)
	}
}

// TestCreateServerGolden asserts every field of the create request.
func TestCreateServerGolden(t *testing.T) {
	e := newEnv(t, fakeOpts())
	t.Run("defaults", func(t *testing.T) {
		r, err := BuildCreateServer(e.spec("nc-abc", "1xCPU-2GB"))
		if err != nil {
			t.Fatal(err)
		}
		golden(t, "createserver_defaults.golden.json", r)
	})
	t.Run("everything", func(t *testing.T) {
		s := e.spec("nc-xyz", "2xCPU-4GB")
		s.Root.SizeGB = 64
		s.Data = []DataVolume{
			{Name: "scratch", Tier: "maxiops", SizeGB: 100, Encrypted: true},
			{Name: "cold", Tier: "hdd", SizeGB: 500},
		}
		s.Networking = Networking{PublicIPv4: true, PublicIPv6: true, PrivateNetwork: "03abcdef-0000-4000-8000-000000000001"}
		s.ServerGroup = "04abcdef-0000-4000-8000-000000000002"
		r, err := BuildCreateServer(s)
		if err != nil {
			t.Fatal(err)
		}
		golden(t, "createserver_everything.golden.json", r)
	})
}

func TestCreateServerFixedFields(t *testing.T) {
	e := newEnv(t, fakeOpts())
	r, err := BuildCreateServer(e.spec("nc-abc", "1xCPU-2GB"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Hostname != "nc-abc" || r.Title != "nc-abc" || r.Plan != "1xCPU-2GB" || r.Zone != "fi-hel1" {
		t.Errorf("identity: %+v", r)
	}
	if r.Metadata != upcloud.True || r.Firewall != "on" || r.PasswordDelivery != "none" ||
		r.LoginUser == nil || r.LoginUser.CreatePassword != "no" || r.SimpleBackup != "no" || r.TimeZone != "UTC" ||
		r.RemoteAccessEnabled != upcloud.False {
		t.Errorf("fixed fields: %+v", r)
	}
	if r.UserData != `{"ignition":{"version":"3.4.0"}}` {
		t.Errorf("user data = %q: it must be the raw body, not a URL", r.UserData)
	}
	if got := len(r.Networking.Interfaces); got != 2 {
		t.Errorf("interfaces = %d, want public and utility", got)
	}
}

func TestLaunchSpecValidation(t *testing.T) {
	e := newEnv(t, fakeOpts())
	base := func() LaunchSpec { return e.spec("nc-abc", "1xCPU-2GB") }
	for name, tc := range map[string]struct {
		mod   func(*LaunchSpec)
		class vendors.Class
		code  string
	}{
		"no zone":            {func(s *LaunchSpec) { s.Zone = "" }, vendors.Fatal, ""},
		"upper case name":    {func(s *LaunchSpec) { s.Name = "NC" }, vendors.Fatal, "InvalidName"},
		"gpu plan":           {func(s *LaunchSpec) { s.Plan = Plan{Name: "GPU-8xCPU-64GB-1xL40S", Family: FamilyGPU} }, vendors.Unsupported, ""},
		"unknown family":     {func(s *LaunchSpec) { s.Plan = Plan{Name: "X"} }, vendors.Unsupported, ""},
		"no template":        {func(s *LaunchSpec) { s.Template.UUID = "" }, vendors.Fatal, ""},
		"no user data":       {func(s *LaunchSpec) { s.UserData = nil }, vendors.Fatal, ""},
		"tier mismatch":      {func(s *LaunchSpec) { s.Root.Tier = "standard" }, vendors.Unsupported, "TierMismatch"},
		"root below image":   {func(s *LaunchSpec) { s.Root.SizeGB = 5 }, vendors.Unsupported, "RootSize"},
		"root too large":     {func(s *LaunchSpec) { s.Root.SizeGB = 2000 }, vendors.Unsupported, "RootSize"},
		"too many devices":   {func(s *LaunchSpec) { s.Data = make([]DataVolume, 4) }, vendors.Unsupported, "TooManyDevices"},
		"tiny volume":        {func(s *LaunchSpec) { s.Data = []DataVolume{{Name: "a", Tier: "maxiops", SizeGB: 5}} }, vendors.Unsupported, "DataVolume"},
		"unnamed volume":     {func(s *LaunchSpec) { s.Data = []DataVolume{{Tier: "maxiops", SizeGB: 20}} }, vendors.Unsupported, "DataVolume"},
		"unknown tier":       {func(s *LaunchSpec) { s.Data = []DataVolume{{Name: "a", Tier: "nvme", SizeGB: 20}} }, vendors.Unsupported, "DataVolume"},
		"no public no route": {func(s *LaunchSpec) { s.Networking = Networking{} }, vendors.Unsupported, "NoRoute"},
	} {
		t.Run(name, func(t *testing.T) {
			s := base()
			tc.mod(&s)
			_, err := BuildCreateServer(s)
			if vendors.ClassOf(err) != tc.class || vendors.CodeOf(err) != tc.code {
				t.Fatalf("err = %v (class %s code %q), want class %s code %q", err, vendors.ClassOf(err), vendors.CodeOf(err), tc.class, tc.code)
			}
		})
	}
	// Allowed shapes.
	ok := map[string]Networking{
		"private only (user brings a gateway)": {PrivateNetwork: "n"},
		"ipv6 only":                            {PublicIPv6: true},
		"v4 and v6":                            {PublicIPv4: true, PublicIPv6: true},
	}
	for name, n := range ok {
		s := base()
		s.Networking = n
		if _, err := BuildCreateServer(s); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	s := base()
	s.Data = make([]DataVolume, 3)
	for i := range s.Data {
		s.Data[i] = DataVolume{Name: strings.Repeat("v", i+1), Tier: "standard", SizeGB: 10}
	}
	if _, err := BuildCreateServer(s); err != nil {
		t.Errorf("root plus three volumes: %v", err)
	}
}
