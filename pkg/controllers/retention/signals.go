/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package retention

import (
	"sync"
	"time"

	"k8s.io/utils/clock"
)

// Signals holds what the cloud provider tells the controller: a launch that was
// refused for quota in a vendor, region and capacity type. It is a hint with a
// short life (refusalTTL), never a decision input that has to survive a
// restart: the pending pods and the quota tracker say the same thing again.
type Signals struct {
	clk clock.Clock

	mu       sync.Mutex
	refusals map[QuotaScope]refusal
}

type refusal struct {
	at  time.Time
	cpu int64
}

// NewSignals returns empty signals.
func NewSignals(clk clock.Clock) *Signals {
	if clk == nil {
		clk = clock.RealClock{}
	}
	return &Signals{clk: clk, refusals: map[QuotaScope]refusal{}}
}

// ReportQuotaRefusal records that a launch wanting cpu vCPUs was refused for
// quota in the scope.
func (s *Signals) ReportQuotaRefusal(vendor, region, capacityType string, cpu int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := QuotaScope{Vendor: vendor, Region: region, CapacityType: capacityType}
	prev := s.refusals[key]
	s.refusals[key] = refusal{at: s.clk.Now(), cpu: max(cpu, prev.cpu)}
}

// quotaRefusals returns the scopes refused within refusalTTL with the vCPUs
// the largest refused launch wanted, and forgets the older ones.
func (s *Signals) quotaRefusals() map[QuotaScope]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[QuotaScope]int64{}
	for key, r := range s.refusals {
		if s.clk.Since(r.at) > refusalTTL {
			delete(s.refusals, key)
			continue
		}
		out[key] = r.cpu
	}
	return out
}

// clear forgets the refusals of a scope whose pressure a release has answered.
func (s *Signals) clear(scope QuotaScope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.refusals, scope)
}
