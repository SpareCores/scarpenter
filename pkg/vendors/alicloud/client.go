/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"context"
	"fmt"
	"os"

	openapimodels "github.com/alibabacloud-go/darabonba-openapi/v2/models"
	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"
	vpc "github.com/alibabacloud-go/vpc-20160428/v7/client"
	"github.com/aliyun/credentials-go/credentials"
)

// ClientOptions configure the real client factory.
type ClientOptions struct {
	// UserAgent is sent on every call ("scarpenter/<version>").
	UserAgent string
	// Credential overrides the environment's credential (tests). By default
	// it is the OIDC role of ALIBABA_CLOUD_OIDC_PROVIDER_ARN, _ROLE_ARN and
	// _OIDC_TOKEN_FILE when all three are set, else the default chain of
	// credentials-go (access key variables, the shared credentials file, the
	// ECS instance RAM role and a credentials URI).
	Credential credentials.Credential
	// ECSEndpoint and VPCEndpoint give the endpoint of a region; nil is the
	// pattern ecs.<region>.aliyuncs.com and vpc.<region>.aliyuncs.com (whether
	// the China site uses the same pattern is unverified, plan 21 section 3).
	ECSEndpoint func(region string) string
	VPCEndpoint func(region string) string
	// Protocol is "HTTPS" (the default) or "HTTP", for a test server.
	Protocol string
}

// DefaultEndpoint is the endpoint pattern of an Alibaba Cloud service in a
// region.
func DefaultEndpoint(service, region string) string {
	return service + "." + region + ".aliyuncs.com"
}

// defaultCredential is the credential of the environment. When the OIDC role
// variables are all set it is the OIDC role alone: the default chain of
// credentials-go would also try the ECS instance metadata after an OIDC
// failure, stick to it from then on and report only its timeout, hiding the
// OIDC error. Otherwise it is the default chain.
func defaultCredential() (credentials.Credential, error) {
	if os.Getenv("ALIBABA_CLOUD_ROLE_ARN") == "" ||
		os.Getenv("ALIBABA_CLOUD_OIDC_PROVIDER_ARN") == "" ||
		os.Getenv("ALIBABA_CLOUD_OIDC_TOKEN_FILE") == "" {
		return credentials.NewCredential(nil)
	}
	sessionName := os.Getenv("ALIBABA_CLOUD_ROLE_SESSION_NAME")
	if sessionName == "" {
		sessionName = "scarpenter"
	}
	// The unset fields are read from the same variables by the provider.
	return credentials.NewCredential(new(credentials.Config).
		SetType("oidc_role_arn").
		SetRoleSessionName(sessionName))
}

// NewClientFactory returns the factory of the real clients. It makes no call:
// the SDK clients are built here and reach out only when a method is called,
// and the factory is meant to be wrapped by the driver in a per-region cache
// (the clients are created lazily, one pair per region).
func NewClientFactory(o ClientOptions) ClientFactory {
	return func(_ context.Context, region string) (Clients, error) {
		if region == "" {
			return Clients{}, fmt.Errorf("alicloud: a region is required to create the clients")
		}
		cred := o.Credential
		if cred == nil {
			var err error
			cred, err = defaultCredential()
			if err != nil {
				return Clients{}, fmt.Errorf("alicloud: credentials: %w", err)
			}
		}
		mk := func(service string, fn func(string) string) *openapimodels.Config {
			endpoint := DefaultEndpoint(service, region)
			if fn != nil {
				endpoint = fn(region)
			}
			cfg := &openapimodels.Config{}
			cfg.SetCredential(cred).SetRegionId(region).SetEndpoint(endpoint)
			if o.Protocol != "" {
				cfg.SetProtocol(o.Protocol)
			}
			if o.UserAgent != "" {
				cfg.SetUserAgent(o.UserAgent)
			}
			return cfg
		}
		ec, err := ecs.NewClient(mk("ecs", o.ECSEndpoint))
		if err != nil {
			return Clients{}, fmt.Errorf("alicloud: ecs client for %s: %w", region, err)
		}
		vc, err := vpc.NewClient(mk("vpc", o.VPCEndpoint))
		if err != nil {
			return Clients{}, fmt.Errorf("alicloud: vpc client for %s: %w", region, err)
		}
		return Clients{ECS: ecsAdapter{ec}, VPC: vpcAdapter{vc}}, nil
	}
}
