/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import (
	"context"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
)

// waitOp waits for an operation to be DONE: it long-polls the operation of its
// scope (zone, region or global) in rounds of the wait deadline of the API,
// until the operation is done or limit has passed. An operation that is still
// running at the limit is returned as it is (classifyOperation calls that
// Retryable); the deadline of ctx always wins. Every call keeps its deadline:
// the API passed in is the one wrapped by WithTimeouts.
func (d *Driver) waitOp(ctx context.Context, api ComputeAPI, op *computepb.Operation, limit time.Duration,
) (*computepb.Operation, error) {
	deadline := time.Now().Add(limit)
	for op.GetStatus() != computepb.Operation_DONE {
		if err := ctx.Err(); err != nil {
			return op, classify(err, errContext{})
		}
		if !time.Now().Before(deadline) {
			return op, nil
		}
		var (
			next *computepb.Operation
			err  error
		)
		switch {
		case op.GetZone() != "":
			next, err = api.WaitZoneOperation(ctx, &computepb.WaitZoneOperationRequest{
				Project: d.project, Zone: lastSegment(op.GetZone()), Operation: op.GetName()})
		case op.GetRegion() != "":
			next, err = api.WaitRegionOperation(ctx, &computepb.WaitRegionOperationRequest{
				Project: d.project, Region: lastSegment(op.GetRegion()), Operation: op.GetName()})
		default:
			next, err = api.WaitGlobalOperation(ctx, &computepb.WaitGlobalOperationRequest{
				Project: d.project, Operation: op.GetName()})
		}
		if err != nil {
			return op, classify(err, errContext{})
		}
		op = next
	}
	return op, nil
}

// finish waits for the operation of a mutating call and returns the classified
// error of a failed or unfinished one.
func (d *Driver) finish(ctx context.Context, api ComputeAPI, op *computepb.Operation, ec errContext,
) error {
	op, err := d.waitOp(ctx, api, op, d.launchWait)
	if err != nil {
		return err
	}
	return classifyOperation(op, ec)
}
