/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Command controller runs Karpenter core's operator with SCarpenter's
// CloudProvider. The wiring follows the AWS provider's cmd/controller/main.go
// (Apache-2.0,
// https://github.com/aws/karpenter-provider-aws/blob/30c236cafacb3a23fb3a50370d09d71750b56b29/cmd/controller/main.go),
// adapted to core v1.15.1; the decorators and core's controllers are wired in
// pkg/operator so the tests run the same code.
package main

import (
	coreoperator "sigs.k8s.io/karpenter/pkg/operator"

	"github.com/SpareCores/scarpenter/pkg/cloudprovider"
	"github.com/SpareCores/scarpenter/pkg/operator"
)

func main() {
	ctx, op := operator.NewOperator(coreoperator.NewOperator())
	op.
		WithCoreControllers(ctx, cloudprovider.New()).
		Start(ctx)
}
