/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package sweep

import (
	"context"
	"errors"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
)

type sdkDisks struct{ c *armcompute.DisksClient }

// NewSDKDisks creates the real disk client of a subscription; no call is made.
func NewSDKDisks(cred azcore.TokenCredential, subscription string) (Disks, error) {
	if cred == nil || subscription == "" {
		return nil, errors.New("a credential and a subscription ID are required")
	}
	c, err := armcompute.NewDisksClient(subscription, cred, &arm.ClientOptions{})
	if err != nil {
		return nil, err
	}
	return &sdkDisks{c: c}, nil
}

func (s *sdkDisks) Delete(ctx context.Context, resourceGroup, name string) error {
	p, err := s.c.BeginDelete(ctx, resourceGroup, name, nil)
	if err != nil {
		return err
	}
	_, err = p.PollUntilDone(ctx, &runtime.PollUntilDoneOptions{Frequency: 5 * time.Second})
	return err
}
