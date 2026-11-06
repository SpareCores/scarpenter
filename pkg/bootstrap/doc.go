/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package bootstrap holds everything a Flatcar node needs to join the cluster
// (overview section 8): the bootstrap tokens (token.go), the kubelet
// configuration (kubelet.go) and the rendering of the user-data from embedded
// Butane templates (render.go, a pure function of its input), and the Renderer
// that gathers that input for a NodeClaim (renderer.go). Rendering never
// fetches anything: the artifacts were resolved into the NodeClass status.
package bootstrap
