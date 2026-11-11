/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import (
	"context"
	"time"
)

// Per-call deadlines of the driver (plan 19, "Client layer"): the SDK adds its
// own retry (3 tries on 5xx), the driver's backoff handles throttling.
const (
	CallTimeout   = 45 * time.Second
	CreateTimeout = 90 * time.Second
	// StopTimeout bounds the wait for a hard stop to finish before a delete.
	StopTimeout = 2 * time.Minute
)

// CallContext derives the context of one API call.
func CallContext(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}
