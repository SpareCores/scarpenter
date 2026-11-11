/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	azapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/azure"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/azure/fake"
)

const amd = "kinvolk:flatcar-container-linux-corevm-amd64:stable-gen2:"

func imgReq(arch, channel, version string, use vendors.UseRef) vendors.ImageRequest {
	return vendors.ImageRequest{Region: testRegion, Arch: arch, Channel: channel, Version: version, Use: use}
}

func TestResolveImageDefault(t *testing.T) {
	e := newEnv(t)
	d := e.driver(t)
	ctx := context.Background()
	img, err := d.ResolveImage(ctx, imgReq("amd64", "stable", "4547.1.0", vendors.UseRef{}))
	require.NoError(t, err)
	require.Equal(t, vendors.Image{Ref: amd + "4547.1.0", Channel: "stable", Version: "4547.1.0"}, *img)
	img, err = d.ResolveImage(ctx, imgReq("amd64", "lts", "4081.2.1", vendors.UseRef{}))
	require.NoError(t, err)
	require.Equal(t, "lts", img.Channel)
	_, err = d.ResolveImage(ctx, imgReq("riscv", "stable", "4547.1.0", vendors.UseRef{}))
	require.Equal(t, vendors.Fatal, vendors.ClassOf(err))

	// Answers are cached, the misses too (shorter).
	calls := e.sub.Calls("image.get")
	_, err = d.ResolveImage(ctx, imgReq("amd64", "stable", "4547.1.0", vendors.UseRef{}))
	require.NoError(t, err)
	require.Equal(t, calls, e.sub.Calls("image.get"))
	_, err = d.ResolveImage(ctx, imgReq("amd64", "stable", "1.0.0", vendors.UseRef{}))
	require.True(t, vendors.IsClass(err, vendors.NotFound))
	calls = e.sub.Calls("image.get")
	_, _ = d.ResolveImage(ctx, imgReq("amd64", "stable", "1.0.0", vendors.UseRef{}))
	require.Equal(t, calls, e.sub.Calls("image.get"))

	// A transient failure is not cached.
	e.sub.FailNext("image.get", fake.Error(http.StatusTooManyRequests, "TooManyRequests", "x"), 1)
	_, err = d.ResolveImage(ctx, imgReq("amd64", "stable", "4547.0.0", vendors.UseRef{}))
	require.True(t, vendors.IsClass(err, vendors.Retryable))
	_, err = d.ResolveImage(ctx, imgReq("amd64", "stable", "4547.0.0", vendors.UseRef{}))
	require.NoError(t, err)

	// An image with terms nobody accepted is not ready, with the exact command.
	e.sub.AddImage(DefaultPublisher, OfferAMD64, "beta-gen2", "4600.0.0", planOf(&ImagePlan{Name: "beta-gen2", Product: OfferAMD64, Publisher: DefaultPublisher}))
	_, err = d.ResolveImage(ctx, imgReq("amd64", "beta", "4600.0.0", vendors.UseRef{}))
	require.Equal(t, CodeMarketplaceTermsNotAccepted, vendors.CodeOf(err))
	require.ErrorContains(t, err, "az vm image terms accept --publisher kinvolk --offer "+OfferAMD64+" --plan beta-gen2")
	require.Equal(t, vendors.Fatal, vendors.ClassOf(err))

	// An image of the other architecture in the offer is skipped.
	e.sub.AddImage(DefaultPublisher, OfferAMD64, "alpha-gen2", "4700.0.0", nil)
	e.sub.SetImageArch(DefaultPublisher, OfferAMD64, "alpha-gen2", "4700.0.0", "Arm64")
	_, err = d.ResolveImage(ctx, imgReq("amd64", "alpha", "4700.0.0", vendors.UseRef{}))
	require.True(t, vendors.IsClass(err, vendors.NotFound))
}

func TestResolveImageExpires(t *testing.T) {
	e := newEnv(t)
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC))
	d := New(WithClients(e.clients), WithSubscription("sub-1"), WithPrices(StaticPrices{}))
	d.clock = clk
	require.NoError(t, d.Init(context.Background(), vendors.Config{ClusterName: testCluster, Clock: clk, Log: logr.Discard()}))
	ctx := context.Background()
	_, err := d.ResolveImage(ctx, imgReq("amd64", "stable", "4547.1.0", vendors.UseRef{}))
	require.NoError(t, err)
	calls := e.sub.Calls("image.get")
	clk.Step(30 * time.Minute)
	_, err = d.ResolveImage(ctx, imgReq("amd64", "stable", "4547.1.0", vendors.UseRef{}))
	require.NoError(t, err)
	require.Equal(t, calls, e.sub.Calls("image.get"))
	clk.Step(2 * time.Hour)
	e.sub.FailNext("image.get", fake.Error(http.StatusInternalServerError, "InternalServerError", "x"), 1)
	_, err = d.ResolveImage(ctx, imgReq("amd64", "stable", "4547.1.0", vendors.UseRef{}))
	require.True(t, vendors.IsClass(err, vendors.Retryable), "an expired answer is asked again: %v", err)
}

func TestResolveImageSelected(t *testing.T) {
	e := newEnv(t)
	e.sub.SetImageArch(DefaultPublisher, OfferAMD64, "stable-gen2", "4547.1.0", "x64")
	e.sub.SetImageArch(DefaultPublisher, OfferARM64, "stable", "4547.1.0", "Arm64")
	d := e.driver(t)
	ctx := context.Background()
	e.sub.AddImage(DefaultPublisher, OfferAMD64, "stable-gen2", "4547.10.0", nil)
	use := func(u vendors.UseRef) vendors.ImageRequest { return imgReq("amd64", "stable", "4547.1.0", u) }

	// By URN: the pinned version is returned and says so.
	img, err := d.ResolveImage(ctx, use(vendors.UseRef{IDs: []string{amd + "4547.1.0"}}))
	require.NoError(t, err)
	require.Equal(t, amd+"4547.1.0", img.Ref)
	require.Equal(t, "4547.1.0", img.Version)
	require.Equal(t, "stable", img.Channel)

	// Another release is never returned, whoever named it.
	_, err = d.ResolveImage(ctx, use(vendors.UseRef{IDs: []string{amd + "4547.0.0"}}))
	require.Equal(t, vendors.CodeImageVersionMismatch, vendors.CodeOf(err))
	_, err = d.ResolveImage(ctx, use(vendors.UseRef{IDs: []string{"kinvolk:flatcar-container-linux-corevm-amd64:lts2024-gen2:4081.2.1"}}))
	require.Equal(t, vendors.CodeImageVersionMismatch, vendors.CodeOf(err))
	_, err = d.ResolveImage(ctx, use(vendors.UseRef{IDs: []string{amd + "latest"}}))
	require.True(t, vendors.IsClass(err, vendors.NotFound), "latest is not a version: %v", err)

	// Missing and malformed IDs find nothing.
	_, err = d.ResolveImage(ctx, use(vendors.UseRef{IDs: []string{amd + "9.9.9", "not a urn"}}))
	require.True(t, vendors.IsClass(err, vendors.NotFound), "%v", err)
	// An excluded architecture finds nothing; a list with both is decided by the request.
	_, err = d.ResolveImage(ctx, use(vendors.UseRef{IDs: []string{amd + "4547.1.0"}, Match: common.ResourceMatch{Architecture: []string{"arm64"}}}))
	require.True(t, vendors.IsClass(err, vendors.NotFound))
	arm := "kinvolk:flatcar-container-linux-corevm:stable:4547.1.0"
	got, err := d.ResolveImage(ctx, imgReq("arm64", "stable", "4547.1.0", vendors.UseRef{IDs: []string{amd + "4547.1.0", arm}}))
	require.NoError(t, err)
	require.Equal(t, arm, got.Ref)
	got, err = d.ResolveImage(ctx, imgReq("amd64", "stable", "4547.1.0", vendors.UseRef{IDs: []string{amd + "4547.1.0", arm}}))
	require.NoError(t, err)
	require.Equal(t, amd+"4547.1.0", got.Ref)

	// By a name pattern: the versions of the SKU that match the glob.
	got, err = d.ResolveImage(ctx, use(vendors.UseRef{Name: amd + "4547.1.*"}))
	require.NoError(t, err)
	require.Equal(t, amd+"4547.1.0", got.Ref)
	_, err = d.ResolveImage(ctx, use(vendors.UseRef{Name: amd + "4547.*"}))
	require.Equal(t, vendors.Fatal, vendors.ClassOf(err), "three versions under exactlyOne")
	_, err = d.ResolveImage(ctx, use(vendors.UseRef{Name: amd + "4547.*", Select: common.SelectNewest, OrderBy: "name"}))
	require.Equal(t, vendors.CodeImageVersionMismatch, vendors.CodeOf(err), "the newest by name is another release than the pinned one")
	got, err = d.ResolveImage(ctx, imgReq("amd64", "stable", "4547.10.0", vendors.UseRef{Name: amd + "4547.*", Select: common.SelectNewest, OrderBy: "name"}))
	require.NoError(t, err)
	require.Equal(t, amd+"4547.10.0", got.Ref, "natural order: 4547.10.0 is newer than 4547.1.0")
	_, err = d.ResolveImage(ctx, use(vendors.UseRef{Name: "kinvolk:nothing:stable-gen2:*"}))
	require.True(t, vendors.IsClass(err, vendors.NotFound))

	// What the marketplace cannot do.
	for name, u := range map[string]vendors.UseRef{
		"select all": {IDs: []string{amd + "4547.1.0"}, Select: common.SelectAll},
		"tags":       {Tags: map[string]string{"a": "b"}, Name: "x"},
		"filters":    {Filters: []common.ResourceFilter{{Name: "x", Values: []string{"y"}}}},
		"expression": {Expression: "x"},
		"only match": {Match: common.ResourceMatch{State: []string{"available"}}},
		"a pattern":  {Name: "kinvolk:*:stable-gen2:1"},
		"short name": {Name: "kinvolk:x"},
	} {
		_, err := d.ResolveImage(ctx, use(u))
		require.Equal(t, vendors.Fatal, vendors.ClassOf(err), "%s: %v", name, err)
	}
	many := vendors.UseRef{Name: "kinvolk:many:sku:*"}
	for i := range maxVersionCandidates + 1 {
		e.sub.AddImage("kinvolk", "many", "sku", "1.0."+string(rune('a'+i%26))+string(rune('a'+i/26)), nil)
	}
	_, err = d.ResolveImage(ctx, use(many))
	require.ErrorContains(t, err, "narrow the version glob")
	e.sub.FailNext("image.versions", fake.Error(http.StatusInternalServerError, "InternalServerError", "x"), 1)
	_, err = d.ResolveImage(ctx, use(vendors.UseRef{Name: amd + "4547.2.*"}))
	require.True(t, vendors.IsClass(err, vendors.Retryable))
}

func TestChannelOfSKU(t *testing.T) {
	for sku, want := range map[string]string{
		"stable-gen2": "stable", "beta": "beta", "ALPHA-gen2": "alpha", "lts2024-gen2": "lts", "lts": "lts", "weird": "",
	} {
		require.Equal(t, want, ChannelOfSKU(sku), sku)
	}
	require.Empty(t, ArchOfImage(armImage(nil)))
	x64, arm := armcompute.ArchitectureTypes("x64"), armcompute.ArchitectureTypes("Arm64")
	require.Equal(t, "amd64", ArchOfImage(armImage(&x64)))
	require.Equal(t, "arm64", ArchOfImage(armImage(&arm)))
	unk := armcompute.ArchitectureTypes("other")
	require.Empty(t, ArchOfImage(armImage(&unk)))
}

func TestReportImages(t *testing.T) {
	e := newEnv(t)
	d := e.driver(t)
	ctx := context.Background()
	nc := &v1alpha1.SCNodeClass{}
	nc.Spec.Flatcar = v1alpha1.FlatcarSpec{Channel: "stable", Version: "4547.1.0"}
	nc.Spec.Vendors.Azure = &azapi.Settings{Regions: []string{"westeurope", "northeurope"}}
	// amd64 and arm64 both exist for the version; only westeurope has arm64.
	e.sub.RemoveImageFrom("northeurope", DefaultPublisher, OfferARM64, "stable", "4547.1.0")
	got, err := d.ReportImages(ctx, nc)
	require.NoError(t, err)
	type row struct {
		region, arch string
		ready        bool
	}
	var rows []row
	for _, s := range got {
		rows = append(rows, row{s.Region, s.Arch, s.Ready})
		if s.Ready {
			require.NotEmpty(t, s.Ref)
		} else {
			require.NotEmpty(t, s.Message)
		}
	}
	require.Equal(t, []row{{"northeurope", "amd64", true}, {"northeurope", "arm64", false}, {"westeurope", "amd64", true}, {"westeurope", "arm64", true}}, rows)

	// The terms are the reason of an image that cannot be launched.
	e.sub.AddImage(DefaultPublisher, OfferAMD64, "stable-gen2", "4999.0.0", planOf(&ImagePlan{Name: "stable-gen2", Product: OfferAMD64, Publisher: DefaultPublisher}))
	nc.Spec.Flatcar.Version = "4999.0.0"
	got, err = d.ReportImages(ctx, nc)
	require.NoError(t, err)
	require.Equal(t, CodeMarketplaceTermsNotAccepted, got[0].Reason)

	// A NodeClass selector is read the same way; a version it names that is not
	// the pinned one is the reason.
	nc.Spec.Flatcar.Version = "4547.1.0"
	nc.Spec.Vendors.Azure.Regions = []string{"westeurope"}
	nc.Spec.Vendors.Azure.Resources = &azapi.Resources{Images: &common.Resource{Use: &common.ResourceUse{
		IDs: map[string]common.IDList{"westeurope": {amd + "4547.0.0"}}}}}
	got, err = d.ReportImages(ctx, nc)
	require.NoError(t, err)
	require.Equal(t, vendors.CodeImageVersionMismatch, got[0].Reason)

	// A selector without IDs for the region is an error of the status, never a launch.
	nc.Spec.Vendors.Azure.Resources.Images.Use.IDs = map[string]common.IDList{"northeurope": {"x"}}
	got, err = d.ReportImages(ctx, nc)
	require.NoError(t, err)
	require.False(t, got[0].Ready)

	// Throttling is not an answer: the whole report fails and the status keeps its verdict.
	nc.Spec.Vendors.Azure.Resources = nil
	nc.Spec.Vendors.Azure.Regions = nil
	d2 := e.driver(t)
	e.sub.FailNext("image.get", fake.Error(http.StatusTooManyRequests, "TooManyRequests", "x"), -1)
	_, err = d2.ReportImages(ctx, nc)
	require.Error(t, err)
	e.sub.ClearFailures()

	// Without a list the regions of the subscription are reported.
	got, err = d2.ReportImages(ctx, nc)
	require.NoError(t, err)
	require.Len(t, got, 6)
	e.sub.FailNext("locations.list", fake.Error(http.StatusInternalServerError, "InternalServerError", "x"), 1)
	_, err = e.driver(t).ReportImages(ctx, nc)
	require.Error(t, err)
}

func armImage(arch *armcompute.ArchitectureTypes) armcompute.VirtualMachineImage {
	img := armcompute.VirtualMachineImage{}
	if arch != nil {
		img.Properties = &armcompute.VirtualMachineImageProperties{Architecture: arch}
	}
	return img
}
