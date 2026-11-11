/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	awsapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws/fake"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// imageReq is a request for the pinned release with a selector.
func imageReq(arch string, use vendors.UseRef) vendors.ImageRequest {
	return vendors.ImageRequest{Region: testRegion, Arch: arch, Channel: "stable", Version: "4757.2.3", Use: use}
}

func idsOf(ids ...string) vendors.UseRef {
	return vendors.UseRef{IDs: ids, Select: common.SelectExactlyOne}
}

// userImage is an AMI of the operator's account.
func userImage(id, name, arch string, tags map[string]string) fake.Image {
	return fake.Image{ID: id, Name: name, Owner: "123456789012", Arch: arch, Tags: tags}
}

func TestExplicitImageIDsAreResolvedAndSendTheIDFilter(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	e.acct.AddImage(testRegion, userImage("ami-0aaaaaaaaaaaa001", "copy-of-flatcar", "x86_64",
		map[string]string{"flatcar-channel": "stable", "flatcar-version": "4757.2.3"}))
	e.acct.AddImage(testRegion, userImage("ami-0aaaaaaaaaaaa002", "copy-of-flatcar-arm", "arm64",
		map[string]string{"flatcar-channel": "stable", "flatcar-version": "4757.2.3"}))
	// The list holds both architectures: the request's architecture decides.
	use := idsOf("ami-0aaaaaaaaaaaa001", "ami-0aaaaaaaaaaaa002")
	// In order: the filters checked below are those of the last call, arm64.
	for _, c := range [][2]string{{"amd64", "ami-0aaaaaaaaaaaa001"}, {"arm64", "ami-0aaaaaaaaaaaa002"}} {
		arch, want := c[0], c[1]
		img, err := e.d.ResolveImage(ctx, imageReq(arch, use))
		if err != nil || img.Ref != want || img.Version != "4757.2.3" || img.Channel != "stable" {
			t.Errorf("%s: %+v, %v; want %s of 4757.2.3", arch, img, err, want)
		}
	}
	in := e.acct.Inputs("DescribeImages")
	last := in[len(in)-1].(*ec2.DescribeImagesInput)
	if len(last.Owners) != 0 {
		t.Errorf("Owners = %v: an ID is exact and needs no owner", last.Owners)
	}
	got := map[string][]string{}
	for _, f := range last.Filters {
		got[*f.Name] = f.Values
	}
	if !slices.Equal(got["image-id"], use.IDs) || !slices.Equal(got["architecture"], []string{"arm64"}) ||
		!slices.Equal(got["state"], []string{"available"}) {
		t.Errorf("filters = %v", got)
	}
}

func TestAnImageOfAnotherVersionIsNeverReturned(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	e.acct.AddImage(testRegion, fake.Image{ID: "ami-0bbbbbbbbbbbb001", Name: "Flatcar-stable-4500.1.0-hvm", Owner: "123456789012"})
	_, err := e.d.ResolveImage(ctx, imageReq("amd64", idsOf("ami-0bbbbbbbbbbbb001")))
	if vendors.CodeOf(err) != vendors.CodeImageVersionMismatch || vendors.ClassOf(err) != vendors.Fatal ||
		!strings.Contains(err.Error(), "ami-0bbbbbbbbbbbb001") || !strings.Contains(err.Error(), "4500.1.0") {
		t.Errorf("another version: %v", err)
	}
	// Another channel of the same version is another release too.
	e.acct.AddImage(testRegion, fake.Image{ID: "ami-0bbbbbbbbbbbb002", Name: "Flatcar-beta-4757.2.3-hvm", Owner: "123456789012"})
	_, err = e.d.ResolveImage(ctx, imageReq("amd64", idsOf("ami-0bbbbbbbbbbbb002")))
	if vendors.CodeOf(err) != vendors.CodeImageVersionMismatch {
		t.Errorf("another channel: %v", err)
	}
	// The answer is remembered: asking again is no new call.
	calls := e.acct.Calls("DescribeImages")
	_, _ = e.d.ResolveImage(ctx, imageReq("amd64", idsOf("ami-0bbbbbbbbbbbb002")))
	if e.acct.Calls("DescribeImages") != calls {
		t.Error("a refused image is looked up again at once")
	}
}

func TestAnImageThatDoesNotSayItsVersionIsNotReady(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	e.acct.AddImage(testRegion, userImage("ami-0ccccccccccccc01", "my-golden-image", "x86_64", nil))
	_, err := e.d.ResolveImage(ctx, imageReq("amd64", idsOf("ami-0ccccccccccccc01")))
	if vendors.CodeOf(err) != vendors.CodeImageVersionUnknown || vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("no version: %v", err)
	}
	// A name that is not the official naming and tags with only one half: unknown.
	e.acct.AddImage(testRegion, userImage("ami-0ccccccccccccc02", "Flatcar-latest", "x86_64",
		map[string]string{"flatcar-channel": "stable"}))
	_, err = e.d.ResolveImage(ctx, imageReq("amd64", idsOf("ami-0ccccccccccccc02")))
	if vendors.CodeOf(err) != vendors.CodeImageVersionUnknown {
		t.Errorf("a channel without a version: %v", err)
	}
}

func TestAnImageWhoseNameAndTagsDisagreeIsNotReady(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	// The name says the pinned release, the tags another version: neither wins.
	e.acct.AddImage(testRegion, userImage("ami-0fffffffffffff01", "Flatcar-stable-4757.2.3-hvm", "x86_64",
		map[string]string{"flatcar-version": "4500.1.0"}))
	_, err := e.d.ResolveImage(ctx, imageReq("amd64", idsOf("ami-0fffffffffffff01")))
	if vendors.CodeOf(err) != vendors.CodeImageVersionUnknown || vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("name and version tag disagree: %v", err)
	}
	// Another channel in the tags is a disagreement too.
	e.acct.AddImage(testRegion, userImage("ami-0fffffffffffff02", "Flatcar-stable-4757.2.3-hvm", "x86_64",
		map[string]string{"flatcar-channel": "beta"}))
	_, err = e.d.ResolveImage(ctx, imageReq("amd64", idsOf("ami-0fffffffffffff02")))
	if vendors.CodeOf(err) != vendors.CodeImageVersionUnknown {
		t.Errorf("name and channel tag disagree: %v", err)
	}
	// Tags that agree with the name are fine.
	e.acct.AddImage(testRegion, userImage("ami-0fffffffffffff03", "Flatcar-stable-4757.2.3-hvm", "x86_64",
		map[string]string{"flatcar-channel": "stable", "flatcar-version": "4757.2.3"}))
	if img, err := e.d.ResolveImage(ctx, imageReq("amd64", idsOf("ami-0fffffffffffff03"))); err != nil ||
		img.Ref != "ami-0fffffffffffff03" {
		t.Errorf("name and tags agree: %+v, %v", img, err)
	}
}

func TestAMissingImageIDIsNotFoundAndNamed(t *testing.T) {
	e := newEnv(t, envOptions{})
	_, err := e.d.ResolveImage(context.Background(), imageReq("amd64", idsOf("ami-0ddddddddddddd01")))
	if vendors.CodeOf(err) != "ImageNotFound" || vendors.ClassOf(err) != vendors.NotFound ||
		!strings.Contains(err.Error(), "ami-0ddddddddddddd01") {
		t.Errorf("a missing ID: %v", err)
	}
}

func TestImageSelectorByNameTagsAndOrder(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	mk := func(id, name string, age time.Duration) {
		e.acct.AddImage(testRegion, fake.Image{ID: id, Name: name, Owner: "123456789012", Arch: "x86_64",
			Created: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC).Add(-age),
			Tags:    map[string]string{"flatcar-channel": "stable", "flatcar-version": "4757.2.3", "built-by": "ci"}})
	}
	mk("ami-0eeeeeeeeeeeee01", "flatcar-build-1", 48*time.Hour)
	mk("ami-0eeeeeeeeeeeee02", "flatcar-build-2", 24*time.Hour)
	owned := common.ResourceMatch{Owner: []string{"123456789012"}}
	// Two matches under exactlyOne are ambiguous and named.
	_, err := e.d.ResolveImage(ctx, imageReq("amd64", vendors.UseRef{Name: "flatcar-build-*", Match: owned,
		Select: common.SelectExactlyOne}))
	if vendors.CodeOf(err) != "ImageAmbiguous" || vendors.ClassOf(err) != vendors.Fatal ||
		!strings.Contains(err.Error(), "2 match") {
		t.Errorf("two matches: %v", err)
	}
	// newest picks the younger, oldest the older.
	for sel, want := range map[common.Select]string{common.SelectNewest: "ami-0eeeeeeeeeeeee02", common.SelectOldest: "ami-0eeeeeeeeeeeee01"} {
		img, err := e.d.ResolveImage(ctx, imageReq("amd64", vendors.UseRef{
			Tags: map[string]string{"built-by": "ci"}, Match: owned, Select: sel, OrderBy: "created"}))
		if err != nil || img.Ref != want {
			t.Errorf("%s: %+v, %v; want %s", sel, img, err, want)
		}
	}
	// The owner reaches EC2 as Owners, and tags as filters.
	in := e.acct.Inputs("DescribeImages")
	last := in[len(in)-1].(*ec2.DescribeImagesInput)
	if !slices.Equal(last.Owners, []string{"123456789012"}) {
		t.Errorf("Owners = %v", last.Owners)
	}
	found := false
	for _, f := range last.Filters {
		found = found || (*f.Name == "tag:built-by" && slices.Equal(f.Values, []string{"ci"}))
	}
	if !found {
		t.Errorf("no tag filter in %+v", last.Filters)
	}
	// Without match.owner a name selector is limited to the block's owner, never to
	// every image the account can see: a stranger's image of the same name must not win.
	_, err = e.d.ResolveImage(ctx, imageReq("amd64", vendors.UseRef{Name: "flatcar-build-1", Select: common.SelectExactlyOne}))
	if vendors.CodeOf(err) != "ImageNotFound" {
		t.Errorf("a name outside the default owner: %v", err)
	}
	// match.architecture that excludes the request's finds nothing.
	_, err = e.d.ResolveImage(ctx, imageReq("arm64", vendors.UseRef{Name: "flatcar-build-1", Match: common.ResourceMatch{
		Owner: []string{"123456789012"}, Architecture: []string{"amd64"}}, Select: common.SelectExactlyOne}))
	if vendors.CodeOf(err) != "ImageNotFound" {
		t.Errorf("an excluded architecture: %v", err)
	}
	// createdAfter is applied by the matcher.
	after := metav1.NewTime(time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC))
	img, err := e.d.ResolveImage(ctx, imageReq("amd64", vendors.UseRef{Name: "flatcar-build-*", Select: common.SelectExactlyOne,
		Match: common.ResourceMatch{Owner: []string{"123456789012"}, CreatedAfter: &after}}))
	if err != nil || img.Ref != "ami-0eeeeeeeeeeeee02" {
		t.Errorf("createdAfter: %+v, %v", img, err)
	}
}

func TestDeprecatedImagesNeedIncludeDeprecated(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	im := userImage("ami-0fffffffffffff01", "Flatcar-stable-4757.2.3-hvm", "x86_64", nil)
	im.Deprecated = true
	e.acct.AddImage(testRegion, im)
	use := vendors.UseRef{IDs: []string{im.ID}, Select: common.SelectExactlyOne}
	if _, err := e.d.ResolveImage(ctx, imageReq("amd64", use)); vendors.CodeOf(err) != "ImageNotFound" {
		t.Errorf("a deprecated image without includeDeprecated: %v", err)
	}
	yes := true
	use.Match.IncludeDeprecated = &yes
	if img, err := e.d.ResolveImage(ctx, imageReq("amd64", use)); err != nil || img.Ref != im.ID {
		t.Errorf("includeDeprecated: %+v, %v", img, err)
	}
}

func TestSelectAllIsRefusedForImages(t *testing.T) {
	e := newEnv(t, envOptions{})
	_, err := e.d.ResolveImage(context.Background(), imageReq("amd64", vendors.UseRef{Name: "x", Select: common.SelectAll}))
	if vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("select all: %v", err)
	}
}

// imagesNodeClass is a NodeClass whose aws block has images.use.
func imagesNodeClass(use *common.ResourceUse, regions ...string) *v1alpha1.SCNodeClass {
	nc := nodeClass(regions...)
	nc.Spec.Vendors.AWS.Resources = &awsapi.Resources{Images: &common.Resource{Use: use}}
	return nc
}

func TestReportImagesHonoursTheImagesSelector(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	e.acct.AddImage(testRegion, userImage("ami-0a1a1a1a1a1a1a01", "mine-amd", "x86_64",
		map[string]string{"flatcar-channel": "stable", "flatcar-version": "4757.2.3"}))
	e.acct.AddImage(testRegion, userImage("ami-0a1a1a1a1a1a1a02", "mine-arm", "arm64",
		map[string]string{"flatcar-channel": "stable", "flatcar-version": "4500.0.0"}))
	nc := imagesNodeClass(&common.ResourceUse{IDs: map[string]common.IDList{testRegion: {
		"ami-0a1a1a1a1a1a1a01", "ami-0a1a1a1a1a1a1a02"}}}, testRegion)
	got, err := e.d.ReportImages(ctx, nc)
	if err != nil || len(got) != 2 {
		t.Fatalf("%+v, %v", got, err)
	}
	amd, arm := got[0], got[1]
	if !amd.Ready || amd.Ref != "ami-0a1a1a1a1a1a1a01" {
		t.Errorf("amd64: %+v", amd)
	}
	if arm.Ready || arm.Reason != vendors.CodeImageVersionMismatch || !strings.Contains(arm.Message, "ami-0a1a1a1a1a1a1a02") ||
		!strings.Contains(arm.Message, testRegion) {
		t.Errorf("arm64: %+v", arm)
	}
}

func TestReportImagesOfARegionWithoutIDsIsNotReady(t *testing.T) {
	e := newEnv(t, envOptions{})
	nc := imagesNodeClass(&common.ResourceUse{IDs: map[string]common.IDList{"eu-central-1": {"ami-0a1a1a1a1a1a1a01"}}}, testRegion)
	got, err := e.d.ReportImages(context.Background(), nc)
	if err != nil || len(got) != 2 || got[0].Ready || got[1].Ready || !strings.Contains(got[0].Message, testRegion) {
		t.Errorf("%+v, %v", got, err)
	}
}

func TestImagePlaceholdersAreExpandedPerArchitecture(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.acct.AddImage(testRegion, userImage("ami-0b2b2b2b2b2b2b01", "fc-stable-4757.2.3-amd64", "x86_64",
		map[string]string{"flatcar-channel": "stable", "flatcar-version": "4757.2.3"}))
	e.acct.AddImage(testRegion, userImage("ami-0b2b2b2b2b2b2b02", "fc-stable-4757.2.3-arm64", "arm64",
		map[string]string{"flatcar-channel": "stable", "flatcar-version": "4757.2.3"}))
	nc := imagesNodeClass(&common.ResourceUse{
		Name:  "fc-{channel}-{version}-{arch}",
		Match: &common.ResourceMatch{Owner: []string{"123456789012"}},
	}, testRegion)
	got, err := e.d.ReportImages(context.Background(), nc)
	if err != nil || len(got) != 2 || !got[0].Ready || !got[1].Ready ||
		got[0].Ref != "ami-0b2b2b2b2b2b2b01" || got[1].Ref != "ami-0b2b2b2b2b2b2b02" {
		t.Errorf("%+v, %v", got, err)
	}
	use, given, err := selector.ImageUse(nc, e.d, testRegion, "arm64")
	if err != nil || !given || use.Name != "fc-stable-4757.2.3-arm64" || use.Select != common.SelectExactlyOne {
		t.Errorf("ImageUse = %+v, %v, %v", use, given, err)
	}
}

func TestAnImagesSelectorTheDriverDoesNotSupportIsRejectedByValidation(t *testing.T) {
	e := newEnv(t, envOptions{})
	for name, tc := range map[string]struct {
		use  *common.ResourceUse
		want string
	}{
		"bad AMI ID":      {&common.ResourceUse{IDs: map[string]common.IDList{testRegion: {"i-123"}}}, "not a valid ID"},
		"bad owner":       {&common.ResourceUse{Name: "x", Match: &common.ResourceMatch{Owner: []string{"someone"}}}, "not an account ID"},
		"select all":      {&common.ResourceUse{Name: "x", Select: common.SelectAll}, "select all"},
		"unknown filter":  {&common.ResourceUse{Filters: []common.ResourceFilter{{Name: "rootdevice-type", Values: []string{"ebs"}}}}, "rootdevice-type"},
		"zone":            {&common.ResourceUse{Name: "x-{zone}"}, "{zone}"},
		"no zone matcher": {&common.ResourceUse{Match: &common.ResourceMatch{Zone: []string{"use1-az1"}}}, "match.zone"},
	} {
		err := e.d.ValidateNodeClass(context.Background(), imagesNodeClass(tc.use, testRegion))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	ok := imagesNodeClass(&common.ResourceUse{Name: "fc-{channel}-{version}-{arch}", Filters: []common.ResourceFilter{
		{Name: "root-device-type", Values: []string{"ebs"}}}}, testRegion)
	if err := e.d.ValidateNodeClass(context.Background(), ok); err != nil {
		t.Errorf("a valid images selector: %v", err)
	}
}

func TestTheDefaultImageStillNamesOwnerAndReleaseInTheError(t *testing.T) {
	e := newEnv(t, envOptions{})
	_, err := e.d.ResolveImage(context.Background(), vendors.ImageRequest{Region: testRegion, Arch: "amd64", Channel: "stable", Version: "1.0.0"})
	if err == nil || !strings.Contains(err.Error(), "Flatcar-stable-1.0.0-hvm") || !strings.Contains(err.Error(), aws.DefaultImageOwner) {
		t.Errorf("the default's error: %v", err)
	}
}
