/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"context"
	"strings"
	"testing"
	"time"

	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"
	"github.com/aliyun/credentials-go/credentials"
	"github.com/go-logr/logr"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// hungECS never answers DescribeZones before its context ends.
type hungECS struct{ ECSAPI }

func (hungECS) DescribeZones(ctx context.Context, _ *ecs.DescribeZonesRequest) (*ecs.DescribeZonesResponseBody, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestTheCallTimeoutBoundsEveryCallOfTheCloud(t *testing.T) {
	d := New(WithCallTimeout(20*time.Millisecond), WithClientFactory(
		func(context.Context, string) (Clients, error) { return Clients{ECS: hungECS{}}, nil }))
	if err := d.Init(bg(), vendors.Config{ClusterName: "c", Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := d.Zones(bg(), "cn-hangzhou")
	if vendors.CodeOf(err) != codeTimeout || vendors.ClassOf(err) != vendors.Retryable {
		t.Errorf("Zones = %v, want Retryable %s", err, codeTimeout)
	}
	if !strings.Contains(err.Error(), "DescribeZones") {
		t.Errorf("the error %q does not name the action that timed out", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("the call took %v, the timeout is 20ms", time.Since(start))
	}
	if d.LoadTimeout() != 20*time.Millisecond {
		t.Errorf("LoadTimeout = %v", d.LoadTimeout())
	}
}

func TestTheCallTimeoutComesFromTheEnvironment(t *testing.T) {
	cases := map[string]time.Duration{"": DefaultCallTimeout, "3m": 3 * time.Minute}
	for env, want := range cases {
		t.Setenv(CallTimeoutEnv, env)
		d := New()
		if err := d.Init(bg(), vendors.Config{ClusterName: "c", Log: logr.Discard()}); err != nil {
			t.Fatal(err)
		}
		if d.LoadTimeout() != want || d.Cloud().callTimeout != want {
			t.Errorf("%q: timeout = %v, want %v", env, d.LoadTimeout(), want)
		}
	}
	for _, bad := range []string{"soon", "0s", "-1m"} {
		t.Setenv(CallTimeoutEnv, bad)
		if err := New().Init(bg(), vendors.Config{ClusterName: "c", Log: logr.Discard()}); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// An endpoint that does not accept connections fails at the connect timeout,
// long before the call timeout.
func TestAnUnreachableEndpointFailsAtTheConnectTimeout(t *testing.T) {
	f := NewClientFactory(ClientOptions{
		Protocol: "HTTP", ConnectTimeout: 300 * time.Millisecond, Credential: testCredential(t),
		ECSEndpoint: func(string) string { return "192.0.2.1:81" }, // TEST-NET-1, never routed
	})
	cl, err := f(bg(), "cn-hangzhou")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = cl.ECS.DescribeZones(bg(), &ecs.DescribeZonesRequest{})
	if err == nil {
		t.Fatal("DescribeZones of an unreachable endpoint succeeded")
	}
	if took := time.Since(start); took > 20*time.Second {
		t.Errorf("failed after %v, want the connect timeout of 300ms", took)
	}
}

func testCredential(t *testing.T) credentials.Credential {
	t.Helper()
	cred, err := credentials.NewCredential(new(credentials.Config).
		SetType("access_key").SetAccessKeyId("test-id").SetAccessKeySecret("test-secret"))
	if err != nil {
		t.Fatal(err)
	}
	return cred
}
