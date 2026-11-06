//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package main

import (
	"context"

	"github.com/SpareCores/scarpenter/pkg/operator"
	"github.com/SpareCores/scarpenter/pkg/vendors/fake"
)

// The development build compiles the fake driver in (overview section 13.2) and
// runs the dev NodeClass status controller that stands in for the NodeClass
// controller of plan 05.
func init() {
	operator.DevHooks = append(operator.DevHooks, func(
		_ context.Context, o *operator.Operator, c *operator.Components,
	) error {
		return fake.RegisterStatusController(o.Manager, c.Catalog, c.Registry)
	})
}
