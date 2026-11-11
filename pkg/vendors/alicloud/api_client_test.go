/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"
	vpc "github.com/alibabacloud-go/vpc-20160428/v7/client"
	"github.com/aliyun/credentials-go/credentials"
)

// TestAdaptersCallTheSDK drives every method of the two adapters through the real
// generated clients against a local server: the signed request carries the action
// the method names, the user agent is ours, and the response body comes back.
// Nothing leaves the machine and no credential is real.
func TestAdaptersCallTheSDK(t *testing.T) {
	var mu sync.Mutex
	var actions []string
	var agents []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		actions = append(actions, r.Header.Get("x-acs-action"))
		agents = append(agents, r.Header.Get("User-Agent"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"RequestId":"req"}`))
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	cred, err := credentials.NewCredential(&credentials.Config{
		Type: new("access_key"), AccessKeyId: new("test-id"), AccessKeySecret: new("test-secret"),
	})
	if err != nil {
		t.Fatal(err)
	}
	factory := NewClientFactory(ClientOptions{
		UserAgent: "scarpenter/test", Credential: cred, Protocol: "HTTP",
		ECSEndpoint: func(string) string { return host }, VPCEndpoint: func(string) string { return host },
	})
	cl, err := factory(context.Background(), "ap-southeast-1")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	calls := map[string]func() error{
		"DescribeRegions": func() error { _, err := cl.ECS.DescribeRegions(ctx, &ecs.DescribeRegionsRequest{}); return err },
		"DescribeZones": func() error {
			_, err := cl.ECS.DescribeZones(ctx, &ecs.DescribeZonesRequest{RegionId: new("r")})
			return err
		},
		"DescribeInstanceTypes": func() error {
			_, err := cl.ECS.DescribeInstanceTypes(ctx, &ecs.DescribeInstanceTypesRequest{})
			return err
		},
		"DescribeAvailableResource": func() error {
			_, err := cl.ECS.DescribeAvailableResource(ctx, &ecs.DescribeAvailableResourceRequest{RegionId: new("r")})
			return err
		},
		"DescribeAccountAttributes": func() error {
			_, err := cl.ECS.DescribeAccountAttributes(ctx, &ecs.DescribeAccountAttributesRequest{RegionId: new("r")})
			return err
		},
		"DescribeImages": func() error {
			_, err := cl.ECS.DescribeImages(ctx, &ecs.DescribeImagesRequest{RegionId: new("r")})
			return err
		},
		"RunInstances": func() error {
			_, err := cl.ECS.RunInstances(ctx, &ecs.RunInstancesRequest{RegionId: new("r")})
			return err
		},
		"DescribeInstances": func() error {
			_, err := cl.ECS.DescribeInstances(ctx, &ecs.DescribeInstancesRequest{RegionId: new("r")})
			return err
		},
		"DeleteInstance": func() error {
			_, err := cl.ECS.DeleteInstance(ctx, &ecs.DeleteInstanceRequest{InstanceId: new("i-1")})
			return err
		},
		"DescribeSecurityGroups": func() error {
			_, err := cl.ECS.DescribeSecurityGroups(ctx, &ecs.DescribeSecurityGroupsRequest{RegionId: new("r")})
			return err
		},
		"CreateSecurityGroup": func() error {
			_, err := cl.ECS.CreateSecurityGroup(ctx, &ecs.CreateSecurityGroupRequest{RegionId: new("r")})
			return err
		},
		"AuthorizeSecurityGroup": func() error {
			_, err := cl.ECS.AuthorizeSecurityGroup(ctx, &ecs.AuthorizeSecurityGroupRequest{RegionId: new("r")})
			return err
		},
		"RevokeSecurityGroup": func() error {
			_, err := cl.ECS.RevokeSecurityGroup(ctx, &ecs.RevokeSecurityGroupRequest{RegionId: new("r")})
			return err
		},
		"DeleteSecurityGroup": func() error {
			_, err := cl.ECS.DeleteSecurityGroup(ctx, &ecs.DeleteSecurityGroupRequest{RegionId: new("r")})
			return err
		},
		"DescribeSecurityGroupAttribute": func() error {
			_, err := cl.ECS.DescribeSecurityGroupAttribute(ctx, &ecs.DescribeSecurityGroupAttributeRequest{RegionId: new("r")})
			return err
		},
		"CreateDeploymentSet": func() error {
			_, err := cl.ECS.CreateDeploymentSet(ctx, &ecs.CreateDeploymentSetRequest{RegionId: new("r")})
			return err
		},
		"DescribeDeploymentSets": func() error {
			_, err := cl.ECS.DescribeDeploymentSets(ctx, &ecs.DescribeDeploymentSetsRequest{RegionId: new("r")})
			return err
		},
		"DeleteDeploymentSet": func() error {
			_, err := cl.ECS.DeleteDeploymentSet(ctx, &ecs.DeleteDeploymentSetRequest{RegionId: new("r")})
			return err
		},
		"TagResources": func() error {
			_, err := cl.ECS.TagResources(ctx, &ecs.TagResourcesRequest{RegionId: new("r")})
			return err
		},
		"ListTagResources": func() error {
			_, err := cl.ECS.ListTagResources(ctx, &ecs.ListTagResourcesRequest{RegionId: new("r")})
			return err
		},
		"DescribeVpcs": func() error {
			_, err := cl.VPC.DescribeVpcs(ctx, &vpc.DescribeVpcsRequest{RegionId: new("r")})
			return err
		},
		"CreateVpc": func() error {
			_, err := cl.VPC.CreateVpc(ctx, &vpc.CreateVpcRequest{RegionId: new("r")})
			return err
		},
		"DeleteVpc": func() error {
			_, err := cl.VPC.DeleteVpc(ctx, &vpc.DeleteVpcRequest{RegionId: new("r"), VpcId: new("vpc-1")})
			return err
		},
		"DescribeVSwitches": func() error {
			_, err := cl.VPC.DescribeVSwitches(ctx, &vpc.DescribeVSwitchesRequest{RegionId: new("r")})
			return err
		},
		"CreateVSwitch": func() error {
			_, err := cl.VPC.CreateVSwitch(ctx, &vpc.CreateVSwitchRequest{RegionId: new("r")})
			return err
		},
		"DeleteVSwitch": func() error {
			_, err := cl.VPC.DeleteVSwitch(ctx, &vpc.DeleteVSwitchRequest{RegionId: new("r"), VSwitchId: new("vsw-1")})
			return err
		},
	}
	for name, fn := range calls {
		if err := fn(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(actions) != len(calls) {
		t.Fatalf("the server saw %d calls, want %d", len(actions), len(calls))
	}
	seen := map[string]bool{}
	for _, a := range actions {
		seen[a] = true
	}
	for name := range calls {
		if !seen[name] {
			t.Errorf("no request carried the action %s (saw %v)", name, actions)
		}
	}
	for _, a := range agents {
		if !strings.Contains(a, "scarpenter/test") {
			t.Errorf("the user agent %q lacks scarpenter/test", a)
		}
	}
}

func TestClientFactoryNeedsARegion(t *testing.T) {
	if _, err := NewClientFactory(ClientOptions{})(context.Background(), ""); err == nil {
		t.Error("a client without a region")
	}
}

func TestDefaultEndpoint(t *testing.T) {
	if got := DefaultEndpoint("ecs", "eu-central-1"); got != "ecs.eu-central-1.aliyuncs.com" {
		t.Errorf("DefaultEndpoint = %q", got)
	}
}
