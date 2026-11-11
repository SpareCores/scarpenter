/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package hcloud_test

import "github.com/prometheus/client_golang/prometheus"

// nopRegisterer accepts every collector and keeps none, so that drivers built
// by many tests do not meet in one registry.
type nopRegisterer struct{}

func (nopRegisterer) Register(prometheus.Collector) error  { return nil }
func (nopRegisterer) MustRegister(...prometheus.Collector) {}
func (nopRegisterer) Unregister(prometheus.Collector) bool { return true }
