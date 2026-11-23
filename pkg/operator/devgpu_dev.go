//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package operator

import "os"

// The development build can turn the GPU pending controller off, for the L4 lane's
// GPU-less boot (test/qemu, plan 12 step (d)). The production build has no such switch.

// gpuPendingDev: SCARPENTER_DEV_NO_GPU_PENDING=1 does not register the GPU pending
// controller, so that a node whose check failed keeps its NodeClaim and its VM.
func gpuPendingDev() bool { return os.Getenv("SCARPENTER_DEV_NO_GPU_PENDING") != "1" }
