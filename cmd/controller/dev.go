//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package main

// The development build compiles the fake driver in (overview section 13.2);
// its init registers the driver and its flags. Nothing else differs from the
// production build: the NodeClass controller is the same.
import _ "github.com/SpareCores/scarpenter/pkg/vendors/fake"
