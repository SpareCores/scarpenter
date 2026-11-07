/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws_test

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	awsapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws/fake"
)

// flatcarReleases is testdata/flatcar_ami_regions.json: the regions of the public
// flatcar_production_ami_all.json of both architectures and the stable and LTS
// channels (overview section 15 Q5; no AWS call was made).
type flatcarReleases struct {
	Releases map[string]struct {
		Version string   `json:"version"`
		Regions []string `json:"regions"`
	} `json:"releases"`
}

func loadReleases(t *testing.T) flatcarReleases {
	t.Helper()
	raw, err := os.ReadFile("testdata/flatcar_ami_regions.json")
	if err != nil {
		t.Fatal(err)
	}
	var r flatcarReleases
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// TestFlatcarAMINamingFollowsThePublishedReleases is the check of overview section
// 15 Q5: arm64 and the LTS channel are published for the same regions as amd64
// (the lists are identical per channel), and the driver names the image of every
// channel and architecture the way Flatcar's release pipeline does:
// Flatcar-<channel>-<version>-hvm and Flatcar-<channel>-<version>-arm64-hvm. The
// name pattern itself could not be read from an AMI listing (that needs an AWS
// call); it follows overview section 11.3 and the Flatcar documentation's
// Flatcar-<channel>-* image names, and plan 10 confirms it.
func TestFlatcarAMINamingFollowsThePublishedReleases(t *testing.T) {
	rel := loadReleases(t)
	for _, ch := range []string{"stable", "lts"} {
		amd, arm := rel.Releases[ch+"/amd64"], rel.Releases[ch+"/arm64"]
		if len(amd.Regions) == 0 || !slices.Equal(amd.Regions, arm.Regions) || amd.Version != arm.Version {
			t.Fatalf("%s: amd64 and arm64 are not published alike (%v vs %v)", ch, amd.Regions, arm.Regions)
		}
		if ch == "lts" && len(amd.Regions) <= len(rel.Releases["stable/amd64"].Regions) {
			t.Error("the LTS channel is published in at least the stable channel's regions")
		}
	}
	for key, r := range rel.Releases {
		channel, arch, _ := cut(key)
		for _, region := range r.Regions {
			acct := fake.New(fake.WithRegions(fake.DefaultRegion(region, "x1"), fake.DefaultRegion("us-east-1", "use1")))
			acct.SetInstanceTypes(smallTypes)
			ec2arch := map[string]string{"amd64": "x86_64", "arm64": "arm64"}[arch]
			acct.AddImage(region, flatcarImage(region, ec2arch, channel, r.Version))
			// The other architecture's image of the release is there too: only the
			// right one may match.
			other := map[string]string{"x86_64": "arm64", "arm64": "x86_64"}[ec2arch]
			acct.AddImage(region, flatcarImage(region+"o", other, channel, r.Version))
			e := newEnvOver(t, acct, nil2clock(), envOptions{})
			img, err := e.d.ResolveImage(context.Background(), vendors.ImageRequest{Region: region, Arch: arch, Channel: channel, Version: r.Version})
			if err != nil || img.Ref == "" {
				t.Fatalf("%s %s in %s: %v", channel, arch, region, err)
			}
		}
	}
}

func cut(key string) (channel, arch string, ok bool) {
	for i := range key {
		if key[i] == '/' {
			return key[:i], key[i+1:], true
		}
	}
	return key, "", false
}

func TestImageNames(t *testing.T) {
	for _, tc := range []struct{ channel, arch, want string }{
		{"stable", "amd64", "Flatcar-stable-4757.2.3-hvm"}, {"stable", "arm64", "Flatcar-stable-4757.2.3-arm64-hvm"},
		{"lts", "amd64", "Flatcar-lts-4081.3.12-hvm"}, {"lts", "arm64", "Flatcar-lts-4081.3.12-arm64-hvm"},
		{"beta", "amd64", "Flatcar-beta-4800.1.0-hvm"}, {"alpha", "arm64", "Flatcar-alpha-4900.0.0-arm64-hvm"},
	} {
		version := map[string]string{"stable": "4757.2.3", "lts": "4081.3.12", "beta": "4800.1.0", "alpha": "4900.0.0"}[tc.channel]
		e := newEnv(t, envOptions{})
		e.acct.AddImage(testRegion, flatcarImage(testRegion, map[string]string{"amd64": "x86_64", "arm64": "arm64"}[tc.arch], tc.channel, version))
		if _, err := e.d.ResolveImage(context.Background(), vendors.ImageRequest{Region: testRegion, Arch: tc.arch, Channel: tc.channel, Version: version}); err != nil {
			t.Fatal(err)
		}
		in := e.acct.Inputs("DescribeImages")[0].(*ec2.DescribeImagesInput)
		var name string
		for _, f := range in.Filters {
			if deref(f.Name) == "name" {
				name = f.Values[0]
			}
		}
		if name != tc.want {
			t.Errorf("%s/%s: name filter %q, want %q", tc.channel, tc.arch, name, tc.want)
		}
	}
}

func TestResolveImage(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	img, err := e.d.ResolveImage(ctx, vendors.ImageRequest{Region: testRegion, Arch: "amd64", Channel: "stable", Version: "4757.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	if img.Ref == "" || img.RootDiskBytes != 8<<30 {
		t.Errorf("image %+v, want a ref and the image's 8 GiB root disk", img)
	}
	in := e.acct.Inputs("DescribeImages")[0].(*ec2.DescribeImagesInput)
	if len(in.Owners) != 1 || in.Owners[0] != "075585003325" {
		t.Errorf("owners %v, want the official Flatcar account", in.Owners)
	}
	have := map[string]string{}
	for _, f := range in.Filters {
		have[deref(f.Name)] = f.Values[0]
	}
	if have["state"] != "available" || have["architecture"] != "x86_64" {
		t.Errorf("filters %v", have)
	}
	arm, err := e.d.ResolveImage(ctx, vendors.ImageRequest{Region: testRegion, Arch: "arm64", Channel: "stable", Version: "4757.2.3"})
	if err != nil || arm.Ref == img.Ref {
		t.Errorf("arm64 image %+v, %v; want a different AMI", arm, err)
	}
	if _, err := e.d.ResolveImage(ctx, vendors.ImageRequest{Region: testRegion, Arch: "riscv", Channel: "stable", Version: "4757.2.3"}); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("an unsupported architecture: %v", err)
	}
}

func TestResolveImageNeedsExactlyOneAvailableMatch(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	req := vendors.ImageRequest{Region: testRegion, Arch: "amd64", Channel: "stable", Version: "4757.2.3"}
	// A second image of the same name from another owner does not match.
	e.acct.AddImage(testRegion, fake.Image{ID: "ami-0000000000other", Name: "Flatcar-stable-4757.2.3-hvm", Owner: "999999999999"})
	if _, err := e.d.ResolveImage(ctx, req); err != nil {
		t.Fatalf("an image of another owner counts: %v", err)
	}
	// A second one of the official owner makes it ambiguous.
	e.acct.AddImage(testRegion, fake.Image{ID: "ami-0000000000twin", Name: "Flatcar-stable-4757.2.3-hvm", Owner: aws.DefaultImageOwner})
	e2 := e.restart(t) // an empty cache
	if _, err := e2.d.ResolveImage(ctx, req); vendors.CodeOf(err) != "ImageAmbiguous" || vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("two matches: %v, want Fatal ImageAmbiguous", err)
	}
	// None: another version, an image that is not available yet, another region.
	for name, r := range map[string]vendors.ImageRequest{
		"another version": {Region: testRegion, Arch: "amd64", Channel: "stable", Version: "9999.0.0"},
		"another channel": {Region: testRegion, Arch: "amd64", Channel: "beta", Version: "4757.2.3"},
	} {
		if _, err := e2.d.ResolveImage(ctx, r); vendors.CodeOf(err) != "ImageNotFound" || vendors.ClassOf(err) != vendors.NotFound {
			t.Errorf("%s: %v, want NotFound ImageNotFound", name, err)
		}
	}
	e3 := newEnv(t, envOptions{})
	e3.acct.AddImage(testRegion, fake.Image{ID: "ami-0000000000pend", Name: "Flatcar-stable-1.0.0-hvm", Owner: aws.DefaultImageOwner, State: "pending"})
	if _, err := e3.d.ResolveImage(ctx, vendors.ImageRequest{Region: testRegion, Arch: "amd64", Channel: "stable", Version: "1.0.0"}); vendors.CodeOf(err) != "ImageNotFound" {
		t.Errorf("an image that is not available: %v", err)
	}
}

func TestResolveImageOwnerOverrideAndCache(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	e.acct.AddImage(testRegion, fake.Image{ID: "ami-0000000000mine", Name: "Flatcar-stable-4757.2.3-hvm", Owner: "123456789012"})
	req := vendors.ImageRequest{Region: testRegion, Arch: "amd64", Channel: "stable", Version: "4757.2.3", Owner: "123456789012"}
	img, err := e.d.ResolveImage(ctx, req)
	if err != nil || img.Ref != "ami-0000000000mine" {
		t.Fatalf("owner override: %+v, %v", img, err)
	}
	calls := e.acct.Calls("DescribeImages")
	if _, err := e.d.ResolveImage(ctx, req); err != nil || e.acct.Calls("DescribeImages") != calls {
		t.Errorf("a second resolution made %d calls", e.acct.Calls("DescribeImages")-calls)
	}
	e.clock.Step(61 * time.Minute)
	if _, err := e.d.ResolveImage(ctx, req); err != nil || e.acct.Calls("DescribeImages") != calls+1 {
		t.Errorf("after the TTL the image was not resolved again (%d calls)", e.acct.Calls("DescribeImages")-calls)
	}
}

func nodeClass(regions ...string) *v1alpha1.SCNodeClass {
	nc := &v1alpha1.SCNodeClass{}
	nc.Spec.Flatcar = v1alpha1.FlatcarSpec{Channel: "stable", Version: "4757.2.3"}
	nc.Spec.Vendors.AWS = &awsapi.Settings{Regions: regions}
	return nc
}

func TestReportImages(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	got, err := e.d.ReportImages(ctx, nodeClass())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("%d images, want both architectures in both regions: %+v", len(got), got)
	}
	for _, img := range got {
		if !img.Ready || img.Ref == "" || img.Message != "" {
			t.Errorf("image %+v is not ready", img)
		}
	}
	if got[0].Region != "eu-central-1" || got[0].Arch != "amd64" || got[3].Region != "us-east-1" || got[3].Arch != "arm64" {
		t.Errorf("not sorted by region and architecture: %+v", got)
	}
	// With a region list only those regions are resolved; the answer for a second
	// report comes from the cache.
	calls := e.acct.Calls("DescribeImages")
	got, err = e.d.ReportImages(ctx, nodeClass("us-east-1"))
	if err != nil || len(got) != 2 || e.acct.Calls("DescribeImages") != calls {
		t.Errorf("report for one region: %+v, %v (%d new calls)", got, err, e.acct.Calls("DescribeImages")-calls)
	}
}

func TestReportImagesNamesTheRegionWithoutAnImage(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.acct.AddImage("us-east-1", fake.Image{ID: "ami-0000000000x", Name: "Flatcar-stable-1.2.3-hvm", Owner: aws.DefaultImageOwner})
	nc := nodeClass("us-east-1", "eu-central-1")
	nc.Spec.Flatcar.Version = "1.2.3"
	got, err := e.d.ReportImages(context.Background(), nc)
	if err != nil {
		t.Fatal(err)
	}
	ready := map[string]bool{}
	for _, img := range got {
		ready[img.Region+"/"+img.Arch] = img.Ready
		if !img.Ready && img.Message == "" {
			t.Errorf("%s/%s is not ready and says nothing", img.Region, img.Arch)
		}
	}
	if !ready["us-east-1/amd64"] || ready["us-east-1/arm64"] || ready["eu-central-1/amd64"] {
		t.Errorf("readiness %v", ready)
	}
}

func TestReportImagesOfAnUnusableRegion(t *testing.T) {
	e := newEnv(t, envOptions{regions: []fake.RegionSpec{
		fake.DefaultRegion("us-east-1", "use1"), {Name: "af-south-1", Disabled: true, AZs: fake.DefaultRegion("af-south-1", "afs1").AZs},
	}})
	e.acct.AddImage("us-east-1", flatcarImage("us-east-1", "x86_64", "stable", "4757.2.3"))
	// Without a region list the regions of the account are reported: the opt-in
	// region the account has not enabled is not one of them.
	got, err := e.d.ReportImages(context.Background(), nodeClass())
	if err != nil {
		t.Fatal(err)
	}
	for _, img := range got {
		if img.Region == "af-south-1" {
			t.Errorf("a disabled region is reported: %+v", img)
		}
	}
	// A region the NodeClass names but the account cannot use is not ready.
	got, _ = e.d.ReportImages(context.Background(), nodeClass("af-south-1"))
	if len(got) != 2 || got[0].Ready {
		t.Errorf("a region the account cannot use: %+v", got)
	}
}

func TestAMissingImageIsNotAskedForAtEveryReport(t *testing.T) {
	e := newEnv(t, envOptions{})
	nc := nodeClass("us-east-1")
	nc.Spec.Flatcar.Version = "9.9.9"
	for range 3 {
		got, err := e.d.ReportImages(context.Background(), nc)
		if err != nil || len(got) != 2 || got[0].Ready {
			t.Fatalf("%+v, %v", got, err)
		}
	}
	if n := e.acct.Calls("DescribeImages"); n != 2 {
		t.Errorf("%d DescribeImages calls for three reports of two missing images, want 2", n)
	}
	// The release appears: after the short TTL it is found.
	e.acct.AddImage("us-east-1", flatcarImage("us-east-1", "x86_64", "stable", "9.9.9"))
	e.clock.Step(6 * time.Minute)
	got, _ := e.d.ReportImages(context.Background(), nc)
	if !got[0].Ready {
		t.Errorf("a new image was not found after the TTL: %+v", got)
	}
}
