/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package operator

import (
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/SpareCores/scarpenter/pkg/controllers/interruption"
)

// WithInterruptionControllers registers the interruption controller (ADR-027,
// plan 26); with --interruption=false nothing is registered.
func (o *Operator) WithInterruptionControllers(c *Components) error {
	if !o.Options.Interruption {
		return nil
	}
	return interruption.New(interruption.Options{
		Client:      o.GetClient(),
		Registry:    c.Registry,
		Unavailable: c.Unavailable,
		Clock:       o.Clock,
		Recorder:    o.EventRecorder,
		Margin:      o.Options.InterruptionMargin,
		Registerer:  metrics.Registry,
	}).Register(o.Manager)
}
