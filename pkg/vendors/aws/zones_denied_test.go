/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/smithy-go"
	"github.com/go-logr/logr"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws/fake"
)

// failingZones is an EC2 client whose DescribeAvailabilityZones fails.
type failingZones struct {
	aws.EC2API
	code string
}

func (f failingZones) DescribeAvailabilityZones(context.Context, *ec2.DescribeAvailabilityZonesInput, ...func(*ec2.Options),
) (*ec2.DescribeAvailabilityZonesOutput, error) {
	return nil, &smithy.OperationError{ServiceID: "EC2", OperationName: "DescribeAvailabilityZones",
		Err: &smithy.GenericAPIError{Code: f.code, Message: "no"}}
}

func TestZonesOfAnUnreadableRegionAreRegionUnavailable(t *testing.T) {
	for code, want := range map[string]vendors.Class{
		"UnauthorizedOperation": vendors.RegionUnavailable, "AuthFailure": vendors.RegionUnavailable,
		"OptInRequired": vendors.RegionUnavailable, "UnrecognizedClientException": vendors.RegionUnavailable,
		"InvalidClientTokenId": vendors.RegionUnavailable,
		"RequestLimitExceeded": vendors.Retryable, "InternalError": vendors.Retryable,
		"InvalidParameterValue": vendors.Fatal,
	} {
		t.Run(code, func(t *testing.T) {
			acct := fake.New()
			d := aws.New(aws.WithClientFactory(func(_ context.Context, region string) (aws.Clients, error) {
				return aws.Clients{EC2: failingZones{EC2API: acct.EC2(region), code: code}, Quotas: acct.Quotas(region)}, nil
			}))
			if err := d.Init(context.Background(), vendors.Config{ClusterName: testCluster, Log: logr.Discard()}); err != nil {
				t.Fatal(err)
			}
			_, err := d.Zones(context.Background(), "us-east-1")
			if got := vendors.ClassOf(err); got != want || vendors.CodeOf(err) != code {
				t.Errorf("%s: class %q code %q (%v), want %q", code, got, vendors.CodeOf(err), err, want)
			}
		})
	}
}
