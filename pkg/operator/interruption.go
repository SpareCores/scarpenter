/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package operator

import (
	"fmt"

	"github.com/SpareCores/scarpenter/pkg/controllers/interruption"
)

// WithInterruptionControllers registers the interruption controller and the
// poller of the drivers' interruption notices (ADR-027, plan 26); with
// --interruption=false nothing is registered.
func (o *Operator) WithInterruptionControllers(c *Components) error {
	if c.Interruption == nil {
		return nil
	}
	if err := c.Interruption.Register(o.Manager); err != nil {
		return err //nolint:wrapcheck // Register wraps it
	}
	if err := o.Add(c.Interruption.Poller(interruption.PollerOptions{})); err != nil {
		return fmt.Errorf("adding the interruption poller to the manager: %w", err)
	}
	return nil
}
