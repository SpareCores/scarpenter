/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/azure/fake"
)

func TestResolveImage(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cases := []struct {
		name string
		req  vendors.ImageRequest
		urn  string
	}{
		{"amd64 stable", vendors.ImageRequest{Region: testRegion, Arch: "amd64", Channel: "stable", Version: "4547.1.0"},
			"kinvolk:flatcar-container-linux-corevm-amd64:stable-gen2:4547.1.0"},
		{"arm64 stable", vendors.ImageRequest{Region: testRegion, Arch: "arm64", Channel: "stable", Version: "4547.1.0"},
			"kinvolk:flatcar-container-linux-corevm:stable:4547.1.0"},
		{"lts picks the stream that has the version", vendors.ImageRequest{Region: testRegion, Arch: "amd64", Channel: "lts", Version: "3510.2.8"},
			"kinvolk:flatcar-container-linux-corevm-amd64:lts2023-gen2:3510.2.8"},
		{"lts current stream", vendors.ImageRequest{Region: testRegion, Arch: "amd64", Channel: "lts", Version: "4081.2.1"},
			"kinvolk:flatcar-container-linux-corevm-amd64:lts2024-gen2:4081.2.1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := e.imgs.Resolve(ctx, c.req)
			require.NoError(t, err)
			require.Equal(t, c.urn, r.Image.URN())
			require.Nil(t, r.Plan)
			back, err := ParseURN(c.urn)
			require.NoError(t, err)
			require.Equal(t, r.Image, back)
		})
	}
}

func TestResolveImageNotFound(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for name, req := range map[string]vendors.ImageRequest{
		"unknown version":  {Region: testRegion, Arch: "amd64", Channel: "stable", Version: "1.0.0"},
		"arm64 has no lts": {Region: testRegion, Arch: "arm64", Channel: "lts", Version: "4081.2.1"},
		"lts version none": {Region: testRegion, Arch: "amd64", Channel: "lts", Version: "1.0.0"},
		"unknown channel":  {Region: testRegion, Arch: "amd64", Channel: "nightly", Version: "4547.1.0"},
	} {
		_, err := e.imgs.Resolve(ctx, req)
		require.Equal(t, vendors.NotFound, vendors.ClassOf(err), name)
		require.Equal(t, codeImageNotFound, vendors.CodeOf(err), name)
	}
	_, err := e.imgs.Resolve(ctx, vendors.ImageRequest{Region: testRegion, Arch: "riscv", Channel: "stable", Version: "4547.1.0"})
	require.Equal(t, vendors.Fatal, vendors.ClassOf(err))

	// A version missing in one region only ("ImagesPartial").
	e.sub.RemoveImageFrom("northeurope", DefaultPublisher, OfferAMD64, "stable-gen2", "4547.1.0")
	_, err = e.imgs.Resolve(ctx, vendors.ImageRequest{Region: "northeurope", Arch: "amd64", Channel: "stable", Version: "4547.1.0"})
	require.Equal(t, vendors.NotFound, vendors.ClassOf(err))
	_, err = e.imgs.Resolve(ctx, vendors.ImageRequest{Region: testRegion, Arch: "amd64", Channel: "stable", Version: "4547.1.0"})
	require.NoError(t, err)
}

func TestResolveImagePublisherOverrideAndCache(t *testing.T) {
	e := newEnv(t)
	clk := clocktesting.NewFakeClock(time.Now())
	e.imgs.Clock = clk
	ctx := context.Background()
	e.sub.AddImage("mycorp", OfferAMD64, "stable-gen2", "9.9.9", nil)
	req := vendors.ImageRequest{Region: testRegion, Arch: "amd64", Channel: "stable", Version: "9.9.9", Owner: "mycorp"}
	r, err := e.imgs.Resolve(ctx, req)
	require.NoError(t, err)
	require.Equal(t, "mycorp", r.Image.Publisher)

	gets := e.sub.Calls("image.get")
	_, err = e.imgs.Resolve(ctx, req)
	require.NoError(t, err)
	require.Equal(t, gets, e.sub.Calls("image.get"), "cached for an hour")
	clk.Step(61 * time.Minute)
	_, err = e.imgs.Resolve(ctx, req)
	require.NoError(t, err)
	require.Greater(t, e.sub.Calls("image.get"), gets)

	// Failures are cached too (NotFound), transient errors are not.
	bad := vendors.ImageRequest{Region: testRegion, Arch: "amd64", Channel: "stable", Version: "0.0.1"}
	_, _ = e.imgs.Resolve(ctx, bad)
	gets = e.sub.Calls("image.get")
	_, _ = e.imgs.Resolve(ctx, bad)
	require.Equal(t, gets, e.sub.Calls("image.get"))
	e.sub.FailNext("image.get", fake.Error(429, "TooManyRequests", "x"), 1)
	_, err = e.imgs.Resolve(ctx, vendors.ImageRequest{Region: testRegion, Arch: "arm64", Channel: "stable", Version: "4547.1.0"})
	require.Equal(t, vendors.Retryable, vendors.ClassOf(err))
	_, err = e.imgs.Resolve(ctx, vendors.ImageRequest{Region: testRegion, Arch: "arm64", Channel: "stable", Version: "4547.1.0"})
	require.NoError(t, err)
}

func TestResolveImageWithPlanAndTerms(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plan := &ImagePlan{Name: "stable-gen2", Product: "flatcar-container-linux-free", Publisher: "kinvolk"}
	e.sub.AddImage("kinvolk", "flatcar-container-linux-free", "stable-gen2", "1.2.3", planOf(plan))
	r := &Resolved{Image: ImageRef{Publisher: "kinvolk", Offer: "flatcar-container-linux-free", SKU: "stable-gen2", Version: "1.2.3"}, Plan: plan}

	require.NoError(t, e.imgs.CheckTerms(ctx, &Resolved{}), "no plan, no terms")
	require.NoError(t, e.imgs.CheckTerms(ctx, nil))
	err := e.imgs.CheckTerms(ctx, r)
	require.Equal(t, vendors.Fatal, vendors.ClassOf(err))
	require.Equal(t, CodeMarketplaceTermsNotAccepted, vendors.CodeOf(err))
	require.Contains(t, err.Error(), "az vm image terms accept --publisher kinvolk --offer flatcar-container-linux-free --plan stable-gen2")
	e.sub.AcceptTerms("kinvolk", "flatcar-container-linux-free", "stable-gen2", true)
	require.NoError(t, e.imgs.CheckTerms(ctx, r))
	e.sub.FailNext("agreement.get", fake.Error(403, "AuthorizationFailed", "x"), 1)
	require.Equal(t, vendors.Fatal, vendors.ClassOf(e.imgs.CheckTerms(ctx, r)))

	// ResolveImage reports the plan of an image that has one.
	got, err := e.imgs.Resolve(ctx, vendors.ImageRequest{Region: testRegion, Arch: "amd64", Channel: "stable", Version: "1.2.3", Owner: "kinvolk"})
	require.Equal(t, vendors.NotFound, vendors.ClassOf(err), "the offer is a driver constant: corevm-amd64 has no such version")
	require.Nil(t, got)
	e.sub.AddImage("kinvolk", OfferAMD64, "beta-gen2", "5.0.0", planOf(&ImagePlan{Name: "beta-gen2", Product: OfferAMD64, Publisher: "kinvolk"}))
	got, err = e.imgs.Resolve(ctx, vendors.ImageRequest{Region: testRegion, Arch: "amd64", Channel: "beta", Version: "5.0.0"})
	require.NoError(t, err)
	require.Equal(t, &ImagePlan{Name: "beta-gen2", Product: OfferAMD64, Publisher: "kinvolk"}, got.Plan)
}
