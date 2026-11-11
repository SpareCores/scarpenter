/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package main

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors/alicloud"
	alifake "github.com/SpareCores/scarpenter/pkg/vendors/alicloud/fake"
	awsdriver "github.com/SpareCores/scarpenter/pkg/vendors/aws"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws/fake"
)

// callEC2 calls the method of the guard with an empty input; a panic of the fake on the
// empty input is not what is tested.
func callEC2(t *testing.T, g *roEC2, name string) (err error) {
	t.Helper()
	m := reflect.ValueOf(g).MethodByName(name)
	in := reflect.New(m.Type().In(1).Elem())
	defer func() {
		if r := recover(); r != nil {
			err = nil
		}
	}()
	out := m.Call([]reflect.Value{reflect.ValueOf(context.Background()), in})
	if e, ok := reflect.TypeAssert[error](out[1]); ok && e != nil {
		return e
	}
	return nil
}

// Every method of the driver's EC2 interface is either a read that reaches the inner client
// or a write that is refused before it: the guard cannot let a write through by omission,
// and a method added to the interface fails the build of the wrappers first.
func TestGuardRefusesEveryWriteAndPassesEveryRead(t *testing.T) {
	typ := reflect.TypeFor[awsdriver.EC2API]()
	if typ.NumMethod() < 30 {
		t.Fatalf("the interface has only %d methods: is the reflection broken?", typ.NumMethod())
	}
	for method := range typ.Methods() {
		name := method.Name
		t.Run(name, func(t *testing.T) {
			acct := fake.New()
			a := newAudit()
			g := &roEC2{inner: acct.EC2("us-east-1"), audit: a}
			err := callEC2(t, g, name)
			if isRead(name) {
				if errors.Is(err, errReadOnly) {
					t.Fatalf("the read %s was refused: %v", name, err)
				}
				if got := a.actions(); len(got) != 1 || got[0] != "ec2:"+name {
					t.Fatalf("the read %s was not passed on and recorded: %v", name, got)
				}
				return
			}
			if !errors.Is(err, errReadOnly) {
				t.Fatalf("the write %s was not refused: %v", name, err)
			}
			if n := acct.Calls(name); n != 0 {
				t.Fatalf("the write %s reached the inner client %d times", name, n)
			}
			if got := a.refusals(); len(got) != 1 || got[0] != "ec2:"+name {
				t.Fatalf("the refusal of %s was not recorded: %v", name, got)
			}
			if len(a.actions()) != 0 {
				t.Fatalf("a refused write was recorded as a read: %v", a.actions())
			}
		})
	}
}

func TestGuardedFactoryWrapsBothClients(t *testing.T) {
	acct := fake.New()
	a := newAudit()
	f := readOnly(func(_ context.Context, region string) (awsdriver.Clients, error) {
		return awsdriver.Clients{EC2: acct.EC2(region), Quotas: acct.Quotas(region)}, nil
	}, a)
	c, err := f(context.Background(), "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.EC2.(*roEC2); !ok {
		t.Fatalf("the EC2 client is %T", c.EC2)
	}
	if _, ok := c.Quotas.(*roQuotas); !ok {
		t.Fatalf("the quotas client is %T", c.Quotas)
	}
}

func TestIsRead(t *testing.T) {
	for op, want := range map[string]bool{
		"DescribeInstances": true, "GetServiceQuota": true, "ListServiceQuotas": true,
		"RunInstances": false, "TerminateInstances": false, "CreateTags": false, "DeleteVpc": false,
		"ModifyInstanceAttribute": false, "describe": false, "": false,
	} {
		if got := isRead(op); got != want {
			t.Errorf("isRead(%q) = %t, want %t", op, got, want)
		}
	}
}

// callGuard calls a method of an Alibaba Cloud guard with an empty input; a panic of the fake on
// the empty input is not what is tested.
func callGuard(t *testing.T, g any, name string) (err error) {
	t.Helper()
	m := reflect.ValueOf(g).MethodByName(name)
	in := reflect.New(m.Type().In(1).Elem())
	defer func() {
		if r := recover(); r != nil {
			err = nil
		}
	}()
	out := m.Call([]reflect.Value{reflect.ValueOf(context.Background()), in})
	if e, ok := reflect.TypeAssert[error](out[1]); ok && e != nil {
		return e
	}
	return nil
}

// Every method of the driver's ECS and VPC interfaces is either a read that reaches the inner
// client or a write that is refused before it: the guard cannot let a write through by
// omission, and a method added to an interface fails the build of the wrappers first.
func TestAlicloudGuardRefusesEveryWriteAndPassesEveryRead(t *testing.T) {
	for _, svc := range []struct {
		prefix string
		typ    reflect.Type
		guard  func(*alifake.Cloud, *audit) any
	}{
		{"ecs", reflect.TypeFor[alicloud.ECSAPI](), func(c *alifake.Cloud, a *audit) any { return &roECS{inner: c.ECS("ap-southeast-1"), audit: a} }},
		{"vpc", reflect.TypeFor[alicloud.VPCAPI](), func(c *alifake.Cloud, a *audit) any { return &roVPC{inner: c.VPC("ap-southeast-1"), audit: a} }},
	} {
		if svc.typ.NumMethod() < 5 {
			t.Fatalf("the %s interface has only %d methods: is the reflection broken?", svc.prefix, svc.typ.NumMethod())
		}
		for method := range svc.typ.Methods() {
			name := method.Name
			t.Run(svc.prefix+"/"+name, func(t *testing.T) {
				acct := alifake.New(nil)
				acct.AddRegion("ap-southeast-1", "ap-southeast-1a")
				a := newAudit()
				err := callGuard(t, svc.guard(acct, a), name)
				action := svc.prefix + ":" + name
				if isRead(name) {
					if errors.Is(err, errReadOnly) {
						t.Fatalf("the read %s was refused: %v", name, err)
					}
					if got := a.actions(); len(got) != 1 || got[0] != action {
						t.Fatalf("the read %s was not passed on and recorded: %v", name, got)
					}
					return
				}
				if !errors.Is(err, errReadOnly) {
					t.Fatalf("the write %s was not refused: %v", name, err)
				}
				if n := acct.Calls(name); n != 0 {
					t.Fatalf("the write %s reached the inner client %d times", name, n)
				}
				if got := a.refusals(); len(got) != 1 || got[0] != action {
					t.Fatalf("the refusal of %s was not recorded: %v", name, got)
				}
				if len(a.actions()) != 0 {
					t.Fatalf("a refused write was recorded as a read: %v", a.actions())
				}
			})
		}
	}
}

func TestAlicloudGuardedFactoryWrapsBothClients(t *testing.T) {
	acct := alifake.New(nil)
	acct.AddRegion("ap-southeast-1", "ap-southeast-1a")
	a := newAudit()
	f := readOnlyAlicloud(func(_ context.Context, region string) (alicloud.Clients, error) {
		return alicloud.Clients{ECS: acct.ECS(region), VPC: acct.VPC(region)}, nil
	}, a)
	c, err := f(context.Background(), "ap-southeast-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.ECS.(*roECS); !ok {
		t.Fatalf("the ECS client is %T", c.ECS)
	}
	if _, ok := c.VPC.(*roVPC); !ok {
		t.Fatalf("the VPC client is %T", c.VPC)
	}
}
