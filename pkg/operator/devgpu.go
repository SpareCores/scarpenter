//go:build !scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package operator

// The production build has no development switches (see devgpu_dev.go).

func nvidiaBootstrapDev() bool { return false }

func gpuPendingDev() bool { return true }
