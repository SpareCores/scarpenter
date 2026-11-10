/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package sweep_test

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	awsdriver "github.com/SpareCores/scarpenter/pkg/vendors/aws"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws/sweep"
)

// down is a region whose endpoint does not answer: its instance listing returns err.
type down struct {
	awsdriver.EC2API
	err error
}

func (d down) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return nil, d.err
}

// timeoutErr is what the SDK returns after its retries when the dial times out.
func timeoutErr() error {
	op := &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}
	return &smithy.OperationError{ServiceID: "EC2", OperationName: "DescribeInstances",
		Err: &retry.MaxAttemptsError{Attempt: 3, Err: &url.Error{Op: "Post", URL: "https://ec2.me-south-1.amazonaws.com/", Err: op}}}
}

// runWithDown sweeps with eu-central-1 failing its instance listing with err.
func (e *env) runWithDown(err error, o sweep.Options) *sweep.Report {
	e.t.Helper()
	o.Now = e.clk.Now
	rep, rerr := sweep.Run(ctx, func(_ context.Context, region string) (awsdriver.Clients, error) {
		if region == "eu-central-1" {
			return awsdriver.Clients{EC2: down{e.acct.EC2(region), err}}, nil
		}
		return awsdriver.Clients{EC2: e.acct.EC2(region), Quotas: e.acct.Quotas(region)}, nil
	}, "us-east-2", o)
	if rerr != nil {
		e.t.Fatal(rerr)
	}
	return rep
}

func TestAnUnreachableRegionIsSkippedOnlyWhenAllRegionsAreSwept(t *testing.T) {
	for name, err := range map[string]error{
		"dial timeout":       timeoutErr(),
		"no such host":       &smithy.OperationError{Err: &net.DNSError{Err: "no such host", Name: "ec2.x.amazonaws.com", IsNotFound: true}},
		"connection refused": &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			id := e.launch(ciOld)
			e.clk.Step(3 * time.Hour)
			rep := e.runWithDown(err, sweep.Options{MaxAge: 2 * time.Hour})
			if outcome(rep, "region", "eu-central-1") != sweep.Skipped || rep.Count(sweep.Failed) != 0 {
				t.Errorf("all regions: %+v", rep.Items)
			}
			if i := slices.IndexFunc(rep.Items, func(i sweep.Item) bool { return i.Kind == "region" }); i < 0 ||
				len(rep.Items[i].Reason) < 12 || rep.Items[i].Reason[:12] != "unreachable:" {
				t.Errorf("the reason does not say unreachable: %+v", rep.Items)
			}
			if outcome(rep, "instance", id) != sweep.Deleted {
				t.Errorf("the other regions were not swept: %+v", rep.Items)
			}
			// The same region asked for by name is a failure.
			rep = e.runWithDown(err, sweep.Options{MaxAge: 2 * time.Hour, Regions: []string{"us-east-2", "eu-central-1"}})
			if rep.Count(sweep.Failed) == 0 || rep.Count(sweep.Skipped) != 0 {
				t.Errorf("explicit region: %+v", rep.Items)
			}
		})
	}
}

func TestOtherErrorsOfARegionStayFailures(t *testing.T) {
	for name, err := range map[string]error{
		"service error": &smithy.GenericAPIError{Code: "InternalError", Message: "boom"},
		"unknown error": errors.New("something else"),
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.clk.Step(3 * time.Hour)
			rep := e.runWithDown(err, sweep.Options{MaxAge: 2 * time.Hour})
			if outcome(rep, "instance", "*") != sweep.Failed {
				t.Errorf("not a failure: %+v", rep.Items)
			}
		})
	}
}

func TestACancelledSweepIsNotAnUnreachableRegion(t *testing.T) {
	e := newEnv(t)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	rep, err := sweep.Run(cctx, func(_ context.Context, region string) (awsdriver.Clients, error) {
		return awsdriver.Clients{EC2: down{e.acct.EC2(region), timeoutErr()}}, nil
	}, "us-east-2", sweep.Options{Regions: nil, Now: e.clk.Now})
	if err == nil && rep.Count(sweep.Skipped) != 0 {
		t.Errorf("a cancelled sweep skipped regions as unreachable: %+v", rep.Items)
	}
}

func TestOnlyTheRequestedRegionsAreSwept(t *testing.T) {
	e := newEnv(t)
	use2 := e.launch(ciOld)
	out, err := e.acct.EC2("eu-central-1").RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId: new("ami-1"), InstanceType: "t3a.small", MinCount: new(int32(1)), MaxCount: new(int32(1)),
		Placement:         &types.Placement{AvailabilityZone: new("eu-central-1a")},
		TagSpecifications: spec(types.ResourceTypeInstance, sweep.ClusterTag, ciOld)})
	if err != nil {
		t.Fatal(err)
	}
	e.clk.Step(3 * time.Hour)
	rep := e.sweep(sweep.Options{MaxAge: 2 * time.Hour, Regions: []string{"us-east-2"}})
	if outcome(rep, "instance", use2) != sweep.Deleted {
		t.Errorf("the requested region was not swept: %+v", rep.Items)
	}
	for _, i := range rep.Items {
		if i.Region != "us-east-2" {
			t.Errorf("another region was visited: %+v", i)
		}
	}
	if got := e.acct.InstanceStates("eu-central-1")[*out.Instances[0].InstanceId]; got != types.InstanceStateNameRunning {
		t.Errorf("an instance of a region not asked for was touched: %s", got)
	}
}

func TestParseRegions(t *testing.T) {
	for _, c := range []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{"", nil, false},
		{"all", nil, false},
		{" ALL ", nil, false},
		{"us-east-2", []string{"us-east-2"}, false},
		{" us-east-2, eu-central-1 ,,us-east-2", []string{"us-east-2", "eu-central-1"}, false},
		{"all,us-east-2", nil, true},
	} {
		got, err := sweep.ParseRegions(c.in)
		if (err != nil) != c.wantErr || !slices.Equal(got, c.want) {
			t.Errorf("ParseRegions(%q) = %v, %v; want %v, err=%v", c.in, got, err, c.want, c.wantErr)
		}
	}
}
