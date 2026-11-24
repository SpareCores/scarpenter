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
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws/fake"
)

// copyDest is a region without an official image; every other region of
// copyEnv has one.
const copyDest = "ap-south-2"

var copyRel = vendors.Release{Channel: "stable", Version: "4757.2.3", Arch: "amd64"}

func copyEnv(t *testing.T, copies bool) *env {
	t.Helper()
	e := newEnv(t, envOptions{copies: copies, regions: []fake.RegionSpec{
		fake.DefaultRegion("us-east-1", "use1"), fake.DefaultRegion("eu-central-1", "euc1"),
		fake.DefaultRegion("us-west-2", "usw2"), fake.DefaultRegion(copyDest, "aps2"),
	}})
	e.acct.RemoveImages(copyDest)
	return e
}

func copyTags(cluster string) map[vendors.TagKey]string {
	return map[vendors.TagKey]string{
		vendors.TagCluster: cluster, vendors.TagManagedBy: vendors.ManagedByValue,
		vendors.TagResource: vendors.ResourceKindImageCopy, vendors.TagImageArch: copyRel.Arch,
		vendors.TagImageSource: "us-east-1/ami-source",
	}
}

// startCopy starts a copy of the release into copyDest from the first source.
func startCopy(t *testing.T, e *env, token string) string {
	t.Helper()
	return startCopyOf(t, e, copyRel, token)
}

func startCopyOf(t *testing.T, e *env, rel vendors.Release, token string) string {
	t.Helper()
	srcs, err := e.d.CopySources(t.Context(), rel)
	if err != nil || len(srcs) == 0 {
		t.Fatalf("CopySources = %v, %v", srcs, err)
	}
	ref, err := e.d.StartCopy(t.Context(), srcs[0], copyDest, rel, copyTags(testCluster), token)
	if err != nil {
		t.Fatalf("StartCopy: %v", err)
	}
	return ref
}

// foreignCopy makes the copy another cluster's SCarpenter would have made, behind the driver's back.
func foreignCopy(t *testing.T, e *env) string {
	t.Helper()
	srcs, err := e.d.CopySources(t.Context(), copyRel)
	if err != nil || len(srcs) == 0 {
		t.Fatalf("CopySources = %v, %v", srcs, err)
	}
	var tags []ec2types.Tag
	for k, v := range copyTags("another-cluster") {
		if k != vendors.TagImageSource {
			tags = append(tags, ec2types.Tag{Key: new(e.d.TagKey(k)), Value: new(v)})
		}
	}
	out, err := e.acct.EC2(copyDest).CopyImage(t.Context(), &ec2.CopyImageInput{
		Name: new("scarpenter-flatcar-stable-4757.2.3-amd64-another-cluster"), SourceRegion: &srcs[0].Region,
		SourceImageId: &srcs[0].Ref, TagSpecifications: []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypeImage, Tags: tags}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return *out.ImageId
}

func listCopies(t *testing.T, e *env) []vendors.ImageCopy {
	t.Helper()
	got, err := e.d.ListCopies(t.Context(), []string{copyDest})
	if err != nil {
		t.Fatalf("ListCopies: %v", err)
	}
	return got
}

func finishCopies(e *env) { e.clock.Step(fake.DefaultCopyDuration + time.Second) }

func TestPrimaryAndArchitectures(t *testing.T) {
	e := copyEnv(t, false)
	ctx := t.Context()
	if ref, found, err := e.d.Primary(ctx, testRegion, copyRel); err != nil || !found || !strings.HasPrefix(ref, "ami-") {
		t.Errorf("Primary(%s) = %q, %v, %v; want the official AMI", testRegion, ref, found, err)
	}
	if ref, found, err := e.d.Primary(ctx, copyDest, copyRel); err != nil || found {
		t.Errorf("Primary(%s) = %q, %v, %v; want none and no error", copyDest, ref, found, err)
	}
	archs, err := e.d.Architectures(ctx, copyDest)
	if err != nil || !slices.Equal(archs, []string{"amd64", "arm64"}) {
		t.Errorf("Architectures = %v, %v", archs, err)
	}
	// A throttled lookup is an error, not "none": the manager must not conclude the copy is needed.
	e.acct.Fail(fake.Failure{Op: "DescribeImages", Code: "RequestLimitExceeded", Count: -1})
	e2 := e.restart(t)
	if _, _, err := e2.d.Primary(ctx, copyDest, copyRel); !vendors.IsClass(err, vendors.Retryable) {
		t.Errorf("Primary under throttling = %v; want Retryable", err)
	}
}

func TestCopySourcesFollowThePreferenceList(t *testing.T) {
	e := copyEnv(t, false)
	e.acct.AddImage("eu-central-1", fake.Image{}) // an image of another name is no source
	srcs, err := e.d.CopySources(t.Context(), copyRel)
	if err != nil {
		t.Fatal(err)
	}
	regions := make([]string, 0, len(srcs))
	for _, s := range srcs {
		regions = append(regions, s.Region)
	}
	// us-east-1, us-west-2 and eu-central-1 have the image; ap-south-2 does not.
	if want := []string{"us-east-1", "us-west-2", "eu-central-1"}; !slices.Equal(regions, want) {
		t.Errorf("sources %v, want %v", regions, want)
	}
	before := e.acct.Calls("DescribeImages")
	if _, err := e.d.CopySources(t.Context(), copyRel); err != nil || e.acct.Calls("DescribeImages") != before {
		t.Errorf("the second CopySources made %d calls, want none (cached)", e.acct.Calls("DescribeImages")-before)
	}
}

func TestCopySourcesDoNotCacheAnEmptyAnswer(t *testing.T) {
	e := copyEnv(t, false)
	for _, r := range []string{"us-east-1", "eu-central-1", "us-west-2"} {
		e.acct.RemoveImages(r)
	}
	if srcs, err := e.d.CopySources(t.Context(), copyRel); err != nil || len(srcs) != 0 {
		t.Fatalf("CopySources with no official image = %v, %v", srcs, err)
	}
	e.acct.AddImage("us-east-1", flatcarImage("us-east-1", "x86_64", "stable", "4757.2.3"))
	// The primary lookup remembers a miss for 5 minutes; the sources must not add an hour to it.
	e.clock.Step(6 * time.Minute)
	if srcs, err := e.d.CopySources(t.Context(), copyRel); err != nil || len(srcs) != 1 {
		t.Errorf("CopySources after the release appeared = %v, %v; want it found", srcs, err)
	}
}

// A region that fails for good (an SCP denies it) is no source and not an error: for a
// release nobody published the answer is "none", which the manager reports as
// ImageCopySourceMissing. Only a transient error makes it an unknown.
func TestCopySourcesIgnoreARegionThatFailsForGood(t *testing.T) {
	e := copyEnv(t, false)
	unpublished := vendors.Release{Channel: "stable", Version: "9.9.9", Arch: "amd64"}
	e.acct.Fail(fake.Failure{Op: "DescribeImages", Region: "eu-central-1", Code: "UnauthorizedOperation", Count: -1})
	if srcs, err := e.d.CopySources(t.Context(), unpublished); err != nil || len(srcs) != 0 {
		t.Errorf("CopySources of an unpublished release with a denied region = %v, %v; want none and no error", srcs, err)
	}
	e2 := copyEnv(t, false)
	e2.acct.Fail(fake.Failure{Op: "DescribeImages", Region: "eu-central-1", Code: "RequestLimitExceeded", Count: -1})
	if _, err := e2.d.CopySources(t.Context(), unpublished); !vendors.IsClass(err, vendors.Retryable) {
		t.Errorf("CopySources with a throttled region and no source = %v; want Retryable", err)
	}
	// With a source found the failing region is left out, and the list is kept only briefly.
	e3 := copyEnv(t, false)
	e3.acct.Fail(fake.Failure{Op: "DescribeImages", Region: "eu-central-1", Code: "UnauthorizedOperation", Count: -1})
	srcs, err := e3.d.CopySources(t.Context(), copyRel)
	if err != nil || len(srcs) != 2 {
		t.Fatalf("CopySources with a denied region = %v, %v; want the 2 other sources", srcs, err)
	}
	before := e3.acct.Calls("DescribeImages")
	e3.d.CopySources(t.Context(), copyRel) //nolint:errcheck // cached
	if e3.acct.Calls("DescribeImages") != before {
		t.Error("an incomplete list was not cached at all")
	}
	e3.clock.Step(6 * time.Minute)
	e3.d.CopySources(t.Context(), copyRel) //nolint:errcheck // asked again
	if e3.acct.Calls("DescribeImages") == before {
		t.Error("an incomplete list was kept for longer than a few minutes")
	}
}

func TestStartCopyCallsCopyImageWithTagsAndToken(t *testing.T) {
	e := copyEnv(t, false)
	ref := startCopy(t, e, "token-1")
	in := e.acct.Inputs("CopyImage")[0].(*ec2.CopyImageInput)
	if deref(in.Name) != "scarpenter-flatcar-stable-4757.2.3-amd64-my-cluster" || deref(in.ClientToken) != "token-1" ||
		deref(in.SourceRegion) != "us-east-1" || !strings.HasPrefix(deref(in.SourceImageId), "ami-") {
		t.Errorf("CopyImage input = %+v", in)
	}
	if in.Encrypted != nil || in.KmsKeyId != nil {
		t.Errorf("the copy sets encryption: %v %v", in.Encrypted, in.KmsKeyId)
	}
	wantTags := map[string]string{
		"scarpenter.sparecores.com/cluster": testCluster, "scarpenter.sparecores.com/managed-by": vendors.ManagedByValue,
		"scarpenter.sparecores.com/resource-kind": "image-copy", "scarpenter.sparecores.com/image-arch": "amd64",
		"flatcar-channel": "stable", "flatcar-version": "4757.2.3",
	}
	if len(in.TagSpecifications) != 2 {
		t.Fatalf("tag specifications %+v, want image and snapshot", in.TagSpecifications)
	}
	for _, ts := range in.TagSpecifications {
		got := map[string]string{}
		for _, tg := range ts.Tags {
			got[deref(tg.Key)] = deref(tg.Value)
		}
		if len(got) != len(wantTags) {
			t.Errorf("%s tags %v, want %v (no image-source tag on AWS)", ts.ResourceType, got, wantTags)
		}
		for k, v := range wantTags {
			if got[k] != v {
				t.Errorf("%s tag %s = %q, want %q", ts.ResourceType, k, got[k], v)
			}
		}
	}
	if ts := in.TagSpecifications; ts[0].ResourceType != ec2types.ResourceTypeImage || ts[1].ResourceType != ec2types.ResourceTypeSnapshot {
		t.Errorf("tag specification types %v, %v", ts[0].ResourceType, ts[1].ResourceType)
	}
	snaps := e.acct.Snapshots(copyDest)
	if len(snaps) != 1 {
		t.Fatalf("snapshots %v, want one", snaps)
	}
	for _, tags := range snaps {
		if tags["scarpenter.sparecores.com/cluster"] != testCluster {
			t.Errorf("the snapshot is not tagged at creation: %v", tags)
		}
	}
	if again := startCopy(t, e, "token-1"); again != ref {
		t.Errorf("the same token made %s and %s", ref, again)
	}
	if got := listCopies(t, e); len(got) != 1 {
		t.Errorf("ListCopies = %+v; want one copy for one token", got)
	}
}

func TestListCopiesStatesAndScoping(t *testing.T) {
	e := copyEnv(t, false)
	ref := startCopy(t, e, "t1")
	got := listCopies(t, e)
	if len(got) != 1 || got[0].Ref != ref || got[0].State != vendors.CopyPending || got[0].Release != copyRel ||
		got[0].Region != copyDest || !strings.HasPrefix(got[0].Source, "us-east-1/ami-") || got[0].Created.IsZero() {
		t.Fatalf("pending copy listed as %+v", got)
	}
	finishCopies(e)
	got = listCopies(t, e)
	if len(got) != 1 || got[0].State != vendors.CopyAvailable || !got[0].LastUsed.IsZero() {
		t.Errorf("available copy listed as %+v", got)
	}
	// What is not this cluster's is not listed: another cluster's copy, an image with only some tags,
	// the same tags on an image of another owner.
	foreignCopy(t, e)
	e.acct.AddImage(copyDest, fake.Image{ID: "ami-partial", Name: "partial", Owner: fake.AccountID,
		Tags: map[string]string{"scarpenter.sparecores.com/cluster": testCluster}})
	e.acct.AddImage(copyDest, fake.Image{ID: "ami-foreign", Name: "foreign", Owner: "999999999999", Tags: map[string]string{
		"scarpenter.sparecores.com/cluster": testCluster, "scarpenter.sparecores.com/managed-by": vendors.ManagedByValue,
		"scarpenter.sparecores.com/resource-kind": "image-copy"}})
	if got := listCopies(t, e); len(got) != 1 || got[0].Ref != ref {
		t.Errorf("ListCopies = %+v; want only this cluster's copy", got)
	}
}

func TestListCopiesFailedImage(t *testing.T) {
	e := copyEnv(t, false)
	e.acct.AddImage(copyDest, fake.Image{ID: "ami-failed", Name: "failed", Owner: fake.AccountID, State: "failed",
		StateMessage: "snapshot copy failed", Tags: map[string]string{
			"scarpenter.sparecores.com/cluster": testCluster, "scarpenter.sparecores.com/managed-by": vendors.ManagedByValue,
			"scarpenter.sparecores.com/resource-kind": "image-copy", "flatcar-channel": "stable", "flatcar-version": "4757.2.3"}})
	got := listCopies(t, e)
	if len(got) != 1 || got[0].State != vendors.CopyFailed || !strings.Contains(got[0].Message, "snapshot copy failed") ||
		got[0].Release.Version != "4757.2.3" || got[0].Release.Arch != "amd64" {
		t.Errorf("failed copy listed as %+v", got)
	}
}

func TestListCopiesLastUsedFromTheImageAttribute(t *testing.T) {
	e := copyEnv(t, false)
	ref := startCopy(t, e, "t1")
	finishCopies(e)
	launched := e.clock.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
	e.acct.AddImage(copyDest, fake.Image{ID: "ami-launched", Name: "launched", Owner: fake.AccountID, LastLaunched: launched,
		Tags: map[string]string{
			"scarpenter.sparecores.com/cluster": testCluster, "scarpenter.sparecores.com/managed-by": vendors.ManagedByValue,
			"scarpenter.sparecores.com/resource-kind": "image-copy"}})
	byRef := map[string]vendors.ImageCopy{}
	for _, c := range listCopies(t, e) {
		byRef[c.Ref] = c
	}
	if !byRef["ami-launched"].LastUsed.Equal(launched) || !byRef[ref].LastUsed.IsZero() {
		t.Errorf("last used: launched %v, never launched %v", byRef["ami-launched"].LastUsed, byRef[ref].LastUsed)
	}
	if e.acct.Calls("DescribeImageAttribute") != 2 {
		t.Errorf("%d DescribeImageAttribute calls, want one per available image", e.acct.Calls("DescribeImageAttribute"))
	}
	// A pending image is not asked about, and the answers are kept for an hour.
	startCopyOf(t, e, vendors.Release{Channel: "stable", Version: "4757.2.3", Arch: "arm64"}, "t2")
	before := e.acct.Calls("DescribeImageAttribute")
	listCopies(t, e)
	if n := e.acct.Calls("DescribeImageAttribute") - before; n != 0 {
		t.Errorf("a listing within the hour made %d attribute calls, want none (cached)", n)
	}
	e.clock.Step(2 * time.Hour)
	listCopies(t, e)
	if n := e.acct.Calls("DescribeImageAttribute") - before; n != 3 {
		t.Errorf("a listing after the hour made %d attribute calls, want 3 (the second copy is available by now)", n)
	}
}

func TestListCopiesWithoutTheAttributeStillLists(t *testing.T) {
	e := copyEnv(t, false)
	startCopy(t, e, "t1")
	finishCopies(e)
	e.acct.Fail(fake.Failure{Op: "DescribeImageAttribute", Code: "UnauthorizedOperation", Count: -1})
	if got := listCopies(t, e); len(got) != 1 || got[0].State != vendors.CopyAvailable || !got[0].LastUsed.IsZero() {
		t.Errorf("ListCopies without the attribute = %+v", got)
	}
	for range 5 { // every poll of the manager: one failed read (and one log line) per image and hour
		listCopies(t, e)
	}
	if n := e.acct.Calls("DescribeImageAttribute"); n != 1 {
		t.Errorf("%d attribute reads for 6 listings within the hour, want 1", n)
	}
}

func TestListCopiesErrorsAreClassified(t *testing.T) {
	e := copyEnv(t, false)
	e.acct.Fail(fake.Failure{Op: "DescribeImages", Code: "UnauthorizedOperation"})
	if _, err := e.d.ListCopies(t.Context(), []string{copyDest}); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("ListCopies denied = %v; want Fatal", err)
	}
	e.acct.Fail(fake.Failure{Op: "DescribeImages", Code: "RequestLimitExceeded"})
	if _, err := e.d.ListCopies(t.Context(), []string{copyDest}); !vendors.IsClass(err, vendors.Retryable) {
		t.Errorf("ListCopies throttled = %v; want Retryable", err)
	}
	// An image without the ownership tags is not listed.
	e.acct.AddImage(copyDest, fake.Image{ID: "ami-x", Name: "x", Owner: fake.AccountID})
	if got := listCopies(t, e); len(got) != 0 {
		t.Errorf("ListCopies = %+v", got)
	}
}

func TestStartCopyErrorClasses(t *testing.T) {
	for _, tc := range []struct {
		code string
		want vendors.Class
	}{
		{"ResourceLimitExceeded", vendors.QuotaExceeded},
		{"ConcurrentSnapshotLimitExceeded", vendors.QuotaExceeded},
		{"RequestLimitExceeded", vendors.Retryable},
		{"Throttling", vendors.Retryable},
		{"InternalError", vendors.Retryable},
		{"UnauthorizedOperation", vendors.Fatal},
		{"AuthFailure", vendors.Fatal},
		{"OptInRequired", vendors.Fatal},
		// The source is gone or unreadable, or the token met another source: the manager retries.
		{"InvalidAMIID.NotFound", vendors.Retryable},
		{"InvalidAMIID.Unavailable", vendors.Retryable},
		{"IdempotentParameterMismatch", vendors.Retryable},
		{"SomethingNew", vendors.Fatal},
	} {
		t.Run(tc.code, func(t *testing.T) {
			e := copyEnv(t, false)
			srcs, err := e.d.CopySources(t.Context(), copyRel)
			if err != nil {
				t.Fatal(err)
			}
			e.acct.Fail(fake.Failure{Op: "CopyImage", Code: tc.code})
			_, err = e.d.StartCopy(t.Context(), srcs[0], copyDest, copyRel, copyTags(testCluster), "tok")
			if !vendors.IsClass(err, tc.want) || vendors.CodeOf(err) != tc.code {
				t.Errorf("StartCopy = %v (class %s, code %s); want %s %s", err, vendors.ClassOf(err), vendors.CodeOf(err), tc.want, tc.code)
			}
			if got := listCopies(t, e); len(got) != 0 {
				t.Errorf("a failed start left a copy: %+v", got)
			}
		})
	}
}

func TestCopyInUseAndLastLaunch(t *testing.T) {
	e := copyEnv(t, false)
	ref := startCopy(t, e, "t1")
	finishCopies(e)
	c := listCopies(t, e)[0]
	if inUse, err := e.d.CopyInUse(t.Context(), c); err != nil || inUse {
		t.Fatalf("CopyInUse of an unused copy = %v, %v", inUse, err)
	}
	req := request(copyDest, copyDest+"a", "m7i.large", vendors.CapacityOnDemand)
	req.Image = vendors.Image{Ref: ref}
	inst, err := e.d.CreateInstance(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if inUse, err := e.d.CopyInUse(t.Context(), c); err != nil || !inUse {
		t.Errorf("CopyInUse with a running instance = %v, %v", inUse, err)
	}
	f := e.acct.Inputs("DescribeInstances")
	in := f[len(f)-1].(*ec2.DescribeInstancesInput)
	states := []string{}
	for _, fl := range in.Filters {
		if deref(fl.Name) == "instance-state-name" {
			states = fl.Values
		}
	}
	if slices.Contains(states, "terminated") || !slices.Contains(states, "stopped") || !slices.Contains(states, "shutting-down") {
		t.Errorf("states asked %v: every state but terminated", states)
	}
	e.clock.Step(2 * time.Hour) // past the cached last launch
	if got := listCopies(t, e)[0]; got.LastUsed.IsZero() {
		t.Errorf("the launch left no lastLaunchedTime: %+v", got)
	}
	if err := e.d.DeleteInstance(t.Context(), inst.Ref); err != nil {
		t.Fatal(err)
	}
	e.acct.SetInstanceState(copyDest, inst.Ref.ID(), ec2types.InstanceStateNameTerminated)
	if inUse, err := e.d.CopyInUse(t.Context(), c); err != nil || inUse {
		t.Errorf("CopyInUse after the instance terminated = %v, %v", inUse, err)
	}
	e.acct.Fail(fake.Failure{Op: "DescribeInstances", Code: "RequestLimitExceeded"})
	if _, err := e.d.CopyInUse(t.Context(), c); !vendors.IsClass(err, vendors.Retryable) {
		t.Errorf("CopyInUse throttled = %v; want Retryable", err)
	}
}

func TestDeleteCopyDeregistersWithSnapshots(t *testing.T) {
	e := copyEnv(t, false)
	startCopy(t, e, "t1")
	finishCopies(e)
	c := listCopies(t, e)[0]
	if err := e.d.TouchCopy(t.Context(), c, e.clock.Now()); err != nil {
		t.Errorf("TouchCopy: %v", err)
	}
	if err := e.d.DeleteCopy(t.Context(), c); err != nil {
		t.Fatalf("DeleteCopy: %v", err)
	}
	in := e.acct.Inputs("DeregisterImage")[0].(*ec2.DeregisterImageInput)
	if deref(in.ImageId) != c.Ref || !deref(in.DeleteAssociatedSnapshots) {
		t.Errorf("DeregisterImage input %+v", in)
	}
	if got := listCopies(t, e); len(got) != 0 {
		t.Errorf("the copy is still listed: %+v", got)
	}
	if snaps := e.acct.Snapshots(copyDest); len(snaps) != 0 {
		t.Errorf("snapshots left behind: %v", snaps)
	}
	if err := e.d.DeleteCopy(t.Context(), c); err != nil {
		t.Errorf("a second DeleteCopy = %v; a missing image is not an error", err)
	}
}

func TestDeleteCopyOnlyDeletesThisClustersCopies(t *testing.T) {
	e := copyEnv(t, false)
	foreign := foreignCopy(t, e)
	official, _, _ := e.d.Primary(t.Context(), testRegion, copyRel)
	// Another cluster's copy is the account's own image without our tags: refused. The
	// official image is not the account's: it shows as missing (Owners self), and is left alone.
	err := e.d.DeleteCopy(t.Context(), vendors.ImageCopy{Region: copyDest, Ref: foreign, Release: copyRel})
	if !vendors.IsClass(err, vendors.Fatal) || vendors.CodeOf(err) != "NotACopy" {
		t.Errorf("DeleteCopy of another cluster's copy = %v; want a Fatal refusal", err)
	}
	if err := e.d.DeleteCopy(t.Context(), vendors.ImageCopy{Region: testRegion, Ref: official, Release: copyRel}); err != nil {
		t.Errorf("DeleteCopy of the official image = %v; want it found missing", err)
	}
	in := e.acct.Inputs("DescribeImages")
	if last := in[len(in)-1].(*ec2.DescribeImagesInput); !slices.Equal(last.Owners, []string{"self"}) {
		t.Errorf("DeleteCopy looked up owners %v, want self", last.Owners)
	}
	if n := e.acct.Calls("DeregisterImage"); n != 0 {
		t.Errorf("%d DeregisterImage calls for images that are not ours", n)
	}
}

func TestDeleteCopyErrors(t *testing.T) {
	e := copyEnv(t, false)
	startCopy(t, e, "t1")
	c := listCopies(t, e)[0]
	e.acct.Fail(fake.Failure{Op: "DeregisterImage", Code: "UnauthorizedOperation"})
	if err := e.d.DeleteCopy(t.Context(), c); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("DeleteCopy denied = %v; want Fatal", err)
	}
	e.acct.Fail(fake.Failure{Op: "DeregisterImage", Code: "RequestLimitExceeded"})
	if err := e.d.DeleteCopy(t.Context(), c); !vendors.IsClass(err, vendors.Retryable) {
		t.Errorf("DeleteCopy throttled = %v; want Retryable", err)
	}
	e.acct.Fail(fake.Failure{Op: "DeregisterImage", Code: "InvalidAMIID.NotFound"})
	if err := e.d.DeleteCopy(t.Context(), c); err != nil {
		t.Errorf("DeleteCopy of an image that vanished between the calls = %v", err)
	}
}

// The driver's calls into the copy manager (plan 29, section 4 steps 4 and 5).

func reportReasons(t *testing.T, e *env, regions ...string) map[string]string {
	t.Helper()
	got, err := e.d.ReportImages(t.Context(), nodeClass(regions...))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, st := range got {
		if st.Arch != "amd64" {
			continue
		}
		out[st.Region] = st.Reason
		if st.Ready {
			out[st.Region] = "ready"
		}
	}
	return out
}

func resolveReq(region string) vendors.ImageRequest {
	return vendors.ImageRequest{Region: region, Arch: "amd64", Channel: "stable", Version: "4757.2.3"}
}

func TestResolveImageAsksTheManagerAfterAPrimaryMiss(t *testing.T) {
	e := copyEnv(t, true)
	ctx := t.Context()
	if img, err := e.d.ResolveImage(ctx, resolveReq(testRegion)); err != nil || !strings.HasPrefix(img.Ref, "ami-") {
		t.Fatalf("the region with an official image: %v, %v", img, err)
	}
	// A report only reads: the region is on demand and nothing is copied.
	if got := reportReasons(t, e, testRegion, copyDest); got[testRegion] != "ready" || got[copyDest] != vendors.CodeImageCopyOnDemand {
		t.Errorf("report = %v", got)
	}
	if n := e.acct.Calls("CopyImage"); n != 0 {
		t.Fatalf("a report started %d copies", n)
	}
	// A launch enqueues, and says ImageCopying (a NotFound the launch path skips).
	_, err := e.d.ResolveImage(ctx, resolveReq(copyDest))
	if vendors.CodeOf(err) != vendors.CodeImageCopying || !vendors.IsClass(err, vendors.NotFound) {
		t.Fatalf("a launch in %s = %v; want ImageCopying", copyDest, err)
	}
	if err := e.mgr.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.acct.Calls("CopyImage"); n != 2 {
		t.Errorf("%d copies started, want both architectures", n)
	}
	if got := reportReasons(t, e, copyDest); got[copyDest] != vendors.CodeImageCopying {
		t.Errorf("report while copying = %v", got)
	}
	finishCopies(e)
	if err := e.mgr.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	img, err := e.d.ResolveImage(ctx, resolveReq(copyDest))
	if err != nil {
		t.Fatalf("after the copy: %v", err)
	}
	if !strings.HasPrefix(img.Ref, "ami-") || img.RootDiskBytes != 8<<30 || img.Channel != "stable" || img.Version != "4757.2.3" {
		t.Errorf("the copy as an image: %+v", img)
	}
	if got := reportReasons(t, e, copyDest); got[copyDest] != "ready" {
		t.Errorf("report after the copy = %v", got)
	}
	// A launch from the copy knows its root device, and the answer is not in the driver's cache:
	// the copy deleted by hand is not served again.
	before := e.acct.Calls("DescribeImages")
	if _, err := e.d.ResolveImage(ctx, resolveReq(copyDest)); err != nil {
		t.Fatal(err)
	}
	if n := e.acct.Calls("DescribeImages") - before; n != 0 {
		t.Errorf("a second launch made %d DescribeImages calls, want none (the primary miss and the copy's facts are cached)", n)
	}
}

func TestResolveImageNeverCachesACopy(t *testing.T) {
	e := copyEnv(t, true)
	ctx := t.Context()
	_, _ = e.d.ResolveImage(ctx, resolveReq(copyDest))
	if err := e.mgr.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	finishCopies(e)
	if err := e.mgr.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	img, err := e.d.ResolveImage(ctx, resolveReq(copyDest))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.mgr.Delete(ctx, vendors.ImageCopy{Region: copyDest, Ref: img.Ref, Release: copyRel}, "Idle"); err != nil {
		t.Fatal(err)
	}
	if again, err := e.d.ResolveImage(ctx, resolveReq(copyDest)); err == nil {
		t.Errorf("the deleted copy %s was served again as %+v", img.Ref, again)
	}
}

func TestResolveImageWithCopiesOffIsUnchanged(t *testing.T) {
	e := copyEnv(t, false)
	_, err := e.d.ResolveImage(t.Context(), resolveReq(copyDest))
	if vendors.CodeOf(err) != "ImageNotFound" || !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("ResolveImage without copies = %v; want ImageNotFound", err)
	}
	if got := reportReasons(t, e, copyDest); got[copyDest] != "" {
		t.Errorf("report without copies = %v; want no copy reason", got)
	}
	for _, op := range []string{"CopyImage", "DescribeImageAttribute", "DeregisterImage"} {
		if n := e.acct.Calls(op); n != 0 {
			t.Errorf("%s called %d times with copies off", op, n)
		}
	}
}

func TestResolveImageOfAClassThatNamesImagesNeverCopies(t *testing.T) {
	e := copyEnv(t, true)
	req := resolveReq(copyDest)
	req.Owner = aws.DefaultImageOwner // spelled out: any imageOwner opts out
	if _, err := e.d.ResolveImage(t.Context(), req); vendors.CodeOf(err) != "ImageNotFound" {
		t.Errorf("ResolveImage with imageOwner = %v; want ImageNotFound", err)
	}
	if n := e.acct.Calls("CopyImage"); n != 0 {
		t.Errorf("%d copies for a class with an imageOwner", n)
	}
	_ = e.mgr.Tick(t.Context())
	if n := e.acct.Calls("CopyImage"); n != 0 {
		t.Errorf("%d copies after a tick", n)
	}
}

func TestManagerErrorsOfTheVendorAreNotImageCopyCodes(t *testing.T) {
	e := copyEnv(t, true)
	// The primary lookup works (the miss is its answer) but listing the copies is denied: the error
	// reaches the caller as the vendor's, classified, and the report does not claim a copy state.
	e.acct.Fail(fake.Failure{Op: "DescribeImages", Code: "UnauthorizedOperation", Count: -1,
		Match: func(in any) bool {
			di := in.(*ec2.DescribeImagesInput)
			return slices.Contains(di.Owners, "self")
		}})
	_, err := e.d.ResolveImage(t.Context(), resolveReq(copyDest))
	if !vendors.IsClass(err, vendors.Fatal) || isCopyReason(vendors.CodeOf(err)) {
		t.Errorf("ResolveImage with ListCopies denied = %v (class %s)", err, vendors.ClassOf(err))
	}
	got, rerr := e.d.ReportImages(t.Context(), nodeClass(copyDest))
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, st := range got {
		if st.Ready || isCopyReason(st.Reason) || st.Message == "" {
			t.Errorf("report with ListCopies denied: %+v", st)
		}
	}
}

func isCopyReason(code string) bool {
	return slices.Contains([]string{vendors.CodeImageCopyOnDemand, vendors.CodeImageCopying, vendors.CodeImageCopyFailed,
		vendors.CodeImageCopySourceMissing}, code)
}

func TestACopyRefusedByAWSIsImageCopyFailed(t *testing.T) {
	e := copyEnv(t, true)
	ctx := context.Background()
	e.acct.Fail(fake.Failure{Op: "CopyImage", Code: "OptInRequired", Count: -1})
	_, _ = e.d.ResolveImage(ctx, resolveReq(copyDest))
	if err := e.mgr.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := e.d.ResolveImage(ctx, resolveReq(copyDest))
	if vendors.CodeOf(err) != vendors.CodeImageCopyFailed || !strings.Contains(err.Error(), "OptInRequired") {
		t.Errorf("after a refused copy: %v; want ImageCopyFailed naming the code", err)
	}
	if n := e.acct.Calls("CopyImage"); n > 2 {
		t.Errorf("%d CopyImage calls: a refusal must not be retried at once", n)
	}
}

func TestACopyLimitBacksOff(t *testing.T) {
	e := copyEnv(t, true)
	ctx := context.Background()
	e.acct.Fail(fake.Failure{Op: "CopyImage", Code: "ResourceLimitExceeded"})
	_, _ = e.d.ResolveImage(ctx, resolveReq(copyDest))
	if err := e.mgr.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.acct.Calls("CopyImage"); n != 1 {
		t.Fatalf("%d CopyImage calls after the limit, want 1", n)
	}
	if err := e.mgr.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.acct.Calls("CopyImage"); n != 1 {
		t.Errorf("%d CopyImage calls during the backoff, want 1", n)
	}
	if _, err := e.d.ResolveImage(ctx, resolveReq(copyDest)); vendors.CodeOf(err) != vendors.CodeImageCopying {
		t.Errorf("a limited copy waits queued: %v", err)
	}
	e.clock.Step(2 * time.Minute)
	if err := e.mgr.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.acct.Calls("CopyImage"); n != 3 {
		t.Errorf("%d CopyImage calls after the backoff, want 3 (the retry and the second architecture)", n)
	}
}

func TestStartCopyAdoptsADuplicateName(t *testing.T) {
	e := copyEnv(t, false)
	ref := startCopy(t, e, "token-a")
	// The same name under another token: EC2 answers InvalidAMIName.Duplicate and the driver adopts the copy.
	if again := startCopy(t, e, "token-b"); again != ref {
		t.Errorf("StartCopy under a new token returned %s, want the existing %s", again, ref)
	}
	if got := listCopies(t, e); len(got) != 1 {
		t.Errorf("ListCopies = %+v; want one copy", got)
	}
	// A duplicate name that is not our copy is a refusal.
	e2 := copyEnv(t, false)
	e2.acct.AddImage(copyDest, fake.Image{ID: "ami-squatter", Name: "scarpenter-flatcar-stable-4757.2.3-amd64-my-cluster",
		Owner: fake.AccountID})
	srcs, err := e2.d.CopySources(t.Context(), copyRel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e2.d.StartCopy(t.Context(), srcs[0], copyDest, copyRel, copyTags(testCluster), "tok"); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("StartCopy over a foreign image of the same name = %v; want Fatal", err)
	}
}

func TestStartCopyOfAWithdrawnSourceForgetsTheSources(t *testing.T) {
	e := copyEnv(t, false)
	srcs, err := e.d.CopySources(t.Context(), copyRel)
	if err != nil {
		t.Fatal(err)
	}
	e.acct.RemoveImages("us-east-1")
	if _, err := e.d.StartCopy(t.Context(), srcs[0], copyDest, copyRel, copyTags(testCluster), "tok"); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("StartCopy from a withdrawn image = %v; want Retryable", err)
	}
	srcs, err = e.d.CopySources(t.Context(), copyRel)
	if err != nil || slices.ContainsFunc(srcs, func(s vendors.ImageSource) bool { return s.Region == "us-east-1" }) {
		t.Errorf("CopySources after the refusal = %v, %v; want the withdrawn source gone", srcs, err)
	}
}

func TestDeregisteredAndMissingCopiesAreGone(t *testing.T) {
	e := copyEnv(t, false)
	tags := map[string]string{
		"scarpenter.sparecores.com/cluster": testCluster, "scarpenter.sparecores.com/managed-by": vendors.ManagedByValue,
		"scarpenter.sparecores.com/resource-kind": "image-copy", "flatcar-channel": "stable", "flatcar-version": "4757.2.3"}
	e.acct.AddImage(copyDest, fake.Image{ID: "ami-dereg", Name: "dereg", Owner: fake.AccountID, State: "deregistered", Tags: tags})
	if got := listCopies(t, e); len(got) != 0 {
		t.Errorf("ListCopies = %+v; a deregistered image is gone, not failed", got)
	}
	gone := vendors.ImageCopy{Region: copyDest, Ref: "ami-dereg", Release: copyRel}
	if err := e.d.DeleteCopy(t.Context(), gone); err != nil {
		t.Errorf("DeleteCopy of a deregistered image = %v", err)
	}
	if err := e.d.DeleteCopy(t.Context(), vendors.ImageCopy{Region: copyDest, Ref: "ami-unknown", Release: copyRel}); err != nil {
		t.Errorf("DeleteCopy of an unknown image = %v", err)
	}
	e.acct.Fail(fake.Failure{Op: "DescribeImages", Code: "InvalidAMIID.Unavailable"})
	if err := e.d.DeleteCopy(t.Context(), gone); err != nil {
		t.Errorf("DeleteCopy answered Unavailable = %v", err)
	}
	if n := e.acct.Calls("DeregisterImage"); n != 0 {
		t.Errorf("%d DeregisterImage calls for images that are gone", n)
	}
	e.acct.AddImage(copyDest, fake.Image{ID: "ami-real", Name: "real", Owner: fake.AccountID, Tags: tags})
	e.acct.Fail(fake.Failure{Op: "DeregisterImage", Code: "InvalidAMIID.Unavailable"})
	if err := e.d.DeleteCopy(t.Context(), vendors.ImageCopy{Region: copyDest, Ref: "ami-real", Release: copyRel}); err != nil {
		t.Errorf("DeregisterImage answered Unavailable = %v", err)
	}
}
