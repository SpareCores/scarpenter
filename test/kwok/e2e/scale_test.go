//go:build kwok

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package e2e

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	scalePods = 50
	// Nothing below is tight: the lane measures, the assertions only catch a
	// controller that does not finish (overview 5.7 and 15 Q7).
	scaleTimeout = 15 * time.Minute
)

// memSampler polls the controller's resident and Go heap memory and keeps the
// maxima.
type memSampler struct {
	mu            sync.Mutex
	rss, heap     float64
	stop, stopped chan struct{}
}

func startSampler(t *testing.T) *memSampler {
	s := &memSampler{stop: make(chan struct{}), stopped: make(chan struct{})}
	sample := func() {
		m := metrics(t)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.rss = max(s.rss, m["process_resident_memory_bytes"])
		s.heap = max(s.heap, m["go_memstats_heap_inuse_bytes"])
	}
	sample()
	go func() {
		defer close(s.stopped)
		for {
			select {
			case <-s.stop:
				return
			case <-time.After(2 * time.Second):
				sample()
			}
		}
	}()
	return s
}

func (s *memSampler) finish() (rss, heap float64) {
	close(s.stop)
	<-s.stopped
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rss, s.heap
}

func mib(b float64) string { return fmt.Sprintf("%.0f MiB", b/(1<<20)) }

func gauge(m map[string]float64, name string) float64 {
	for k, v := range m {
		if strings.HasPrefix(k, name+"{") && strings.Contains(k, `vendor="aws"`) {
			return v
		}
	}
	return 0
}

// S10: scale (overview 5.7, 15 Q7). 50 pending pods with distinct exact-type
// selectors in one NodePool, with the NodeClass allowing every region and then
// three. Measures the time to all NodeClaims, to all pods running and the
// controller's memory; asserts only that everything completes.
func TestS10Scale(t *testing.T) {
	three := []string{us1, us2, "eu-west-1"}
	types := takeTypes(t, three, 2, scalePods)

	for _, c := range []struct {
		name    string
		regions []string
	}{{"three-regions", three}, {"all-regions", nil}} {
		t.Run(c.name, func(t *testing.T) {
			class := "s10-" + c.name
			createNodeClass(t, class, c.regions...)
			createNodePool(t, class, class)
			// A settled controller: the catalog of the class is built.
			time.Sleep(20 * time.Second)
			base := metrics(t)
			sampler := startSampler(t)

			start := time.Now()
			for i, typ := range types {
				createPod(t, fmt.Sprintf("%s-%02d", class, i),
					map[string]string{labelType: vendor + "." + typ, labelNodePool: class}, "100m")
			}
			claims := waitClaimsTimed(t, class, scalePods, scaleTimeout)
			toClaims := time.Since(start)
			eventually(t, scaleTimeout, 2*time.Second, "all pods running", func() (bool, string) {
				var pods corev1.PodList
				if err := k8s.List(ctx(), &pods, client.InNamespace("default"), client.MatchingLabels{labelOwner: ownerLabel(t)}); err != nil {
					return false, err.Error()
				}
				running := 0
				for _, p := range pods.Items {
					if p.Status.Phase == corev1.PodRunning {
						running++
					}
				}
				return running == scalePods, fmt.Sprintf("%d of %d running", running, scalePods)
			})
			toRunning := time.Since(start)
			rss, heap := sampler.finish()
			after := metrics(t)

			line := fmt.Sprintf("Q7: %s: %d pods, %d NodeClaims after %s, all pods running after %s; controller memory at rest %s RSS, peak %s RSS / %s Go heap in use; catalog %.0f types, %.0f offerings (before the pods: %s RSS)",
				c.name, scalePods, len(claims), toClaims.Round(100*time.Millisecond), toRunning.Round(100*time.Millisecond),
				mib(base["process_resident_memory_bytes"]), mib(rss), mib(heap),
				gauge(after, "scarpenter_catalog_instance_types"), gauge(after, "scarpenter_catalog_offerings"),
				mib(base["process_resident_memory_bytes"]))
			t.Log(line)
			appendOut(t, "q7.txt", line)
		})
	}
}

// waitClaimsTimed is waitClaims for the scale scenario (a longer poll interval
// keeps the measurement itself light).
func waitClaimsTimed(t *testing.T, pool string, n int, timeout time.Duration) []string {
	t.Helper()
	var names []string
	eventually(t, timeout, 500*time.Millisecond, fmt.Sprintf("%d NodeClaims of %s", n, pool), func() (bool, string) {
		cs, err := listClaims(pool)
		if err != nil {
			return false, err.Error()
		}
		names = names[:0]
		for _, c := range cs {
			names = append(names, c.Name)
		}
		return len(names) >= n, fmt.Sprintf("%d NodeClaims", len(names))
	})
	return names
}
