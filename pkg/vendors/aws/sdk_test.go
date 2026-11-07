/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// The web identity provider builds its STS client from the base configuration:
// without a region in it, calls fail with "Missing Region" on a cluster that does
// not set AWS_REGION.
func TestSDKConfigHasTheHomeRegionAsDefault(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent")
	cfg, err := sdkConfig(context.Background(), "eu-central-1")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Region != "eu-central-1" {
		t.Errorf("region %q, want the home region", cfg.Region)
	}
	if cfg.RetryMode != aws.RetryModeAdaptive {
		t.Errorf("retry mode %q", cfg.RetryMode)
	}
	t.Setenv("AWS_REGION", "us-west-2")
	if cfg, _ := sdkConfig(context.Background(), "eu-central-1"); cfg.Region != "us-west-2" {
		t.Errorf("an explicit AWS_REGION must win, got %q", cfg.Region)
	}
}
