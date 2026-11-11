/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Command controller runs Karpenter core's operator with SCarpenter's
// CloudProvider. The wiring follows the AWS provider's cmd/controller/main.go
// (Apache-2.0,
// https://github.com/aws/karpenter-provider-aws/blob/30c236cafacb3a23fb3a50370d09d71750b56b29/cmd/controller/main.go),
// adapted to core v1.15.1; the decorators, the components and core's
// controllers are wired in pkg/operator so the tests run the same code.
package main

import (
	"fmt"
	"os"

	"github.com/samber/lo"
	"sigs.k8s.io/controller-runtime/pkg/log"
	coreoperator "sigs.k8s.io/karpenter/pkg/operator"

	"github.com/SpareCores/scarpenter/internal/version"
	"github.com/SpareCores/scarpenter/pkg/operator"
)

func main() {
	// Handled before anything is parsed or connected: the release checks run it
	// on the image without a cluster.
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "-version") {
		fmt.Println(version.Get())
		return
	}
	ctx, op := operator.NewOperator(coreoperator.NewOperator())
	components, err := op.NewComponents(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "building the components")
		os.Exit(1)
	}
	lo.Must0(op.WithComponentControllers(ctx, components), "registering the controllers")
	cp := op.NewCloudProvider(components)
	op.WithCoreControllers(ctx, cp)
	lo.Must0(op.WithRetentionControllers(ctx, components, cp), "registering the billing retention controllers")
	op.Start(ctx)
}
