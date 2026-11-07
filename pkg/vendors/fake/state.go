//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Caps of the published lists, which keep the ConfigMap far below 1 MiB.
const (
	maxCalls     = 2000
	maxInstances = 1000
)

// callRecord is one vendor call of the state ConfigMap's calls list.
type callRecord struct {
	Time         time.Time `json:"time"`
	Op           string    `json:"op"`
	Region       string    `json:"region"`
	Zone         string    `json:"zone"`
	InstanceType string    `json:"instanceType"`
	CapacityType string    `json:"capacityType"`
	NodeClaim    string    `json:"nodeClaim"`
	Result       string    `json:"result"`
	// Resource and Kind are set for the calls on vendor resources (ensure,
	// delete).
	Resource string `json:"resource,omitempty"`
	Kind     string `json:"kind,omitempty"`
}

// instanceRecord is one instance of the state ConfigMap's instances list.
type instanceRecord struct {
	Ref          string    `json:"ref"`
	Region       string    `json:"region"`
	Zone         string    `json:"zone"`
	InstanceType string    `json:"instanceType"`
	CapacityType string    `json:"capacityType"`
	State        string    `json:"state"`
	NodeClaim    string    `json:"nodeClaim"`
	CreatedAt    time.Time `json:"createdAt"`
	// UserDataBytes is the size of the user-data of the launch.
	UserDataBytes int `json:"userDataBytes"`
	// Settings is the resolved vendor settings (JSON) the instance was
	// launched with, Profiles the profile applied per kind, Resources the IDs
	// of the prepared resources and Devices the device hints.
	Settings  string            `json:"settings,omitempty"`
	Profiles  map[string]string `json:"profiles,omitempty"`
	Resources []string          `json:"resources,omitempty"`
	Devices   []string          `json:"devices,omitempty"`
}

// retryBackoff bounds the delay between retries of a failed state write.
const maxFlushBackoff = 10 * time.Second

// markDirtyLocked records a change of the published state and schedules a
// debounced write of the state ConfigMap. The caller holds d.mu.
func (d *Driver) markDirtyLocked() {
	d.revision++
	d.dirty = true
	d.scheduleFlushLocked(0)
}

// scheduleFlushLocked arranges one write after the debounce interval (or the
// given retry delay). A failed write is retried with backoff until it
// succeeds: the state is never lost, which the scenarios' revision-acknowledged
// waits rely on.
func (d *Driver) scheduleFlushLocked(retry time.Duration) {
	if d.flushing || d.cfg.Client == nil {
		return
	}
	d.flushing = true
	delay := d.opts.StateInterval
	if delay <= 0 {
		delay = defaultStateInterval
	}
	if retry > 0 {
		delay = retry
	}
	time.AfterFunc(delay, func() {
		d.mu.Lock()
		d.flushing = false
		d.mu.Unlock()
		err := d.Flush(context.Background())
		d.mu.Lock()
		defer d.mu.Unlock()
		if err != nil {
			d.failures++
			d.cfg.Log.Error(err, "writing the fake driver's state ConfigMap, will retry", "failures", d.failures)
			d.dirty = true
			d.scheduleFlushLocked(min(maxFlushBackoff, 100*time.Millisecond<<min(d.failures, 7)))
			return
		}
		d.failures = 0
		if d.dirty {
			d.scheduleFlushLocked(0)
		}
	})
}

// Flush writes the state ConfigMap now if anything changed since the last
// write. The ConfigMap carries the revision it reflects.
func (d *Driver) Flush(ctx context.Context) error {
	if d.cfg.Client == nil {
		return nil
	}
	d.mu.Lock()
	if !d.dirty {
		d.mu.Unlock()
		return nil
	}
	d.dirty = false
	revision := d.revision
	calls := slices.Clone(d.calls)
	instances := make([]instanceRecord, 0, len(d.instances))
	for _, i := range d.instances {
		instances = append(instances, instanceRecord{
			Ref: string(i.Ref), Region: i.Region, Zone: i.Zone, InstanceType: i.Type, CapacityType: i.CapacityType,
			State: i.State, NodeClaim: i.claim, CreatedAt: i.CreatedAt, UserDataBytes: i.userData,
			Settings: i.settings, Profiles: maps.Clone(i.profiles), Resources: slices.Clone(i.resources),
			Devices: slices.Clone(i.devices),
		})
	}
	d.mu.Unlock()
	// After the revision was read: the snapshot may hold newer changes than the
	// revision claims, never older ones.
	resources := d.resourceSnapshot()
	slices.SortFunc(instances, func(a, b instanceRecord) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.Ref, b.Ref)
	})
	if len(instances) > maxInstances {
		instances = instances[len(instances)-maxInstances:]
	}
	callsJSON, err := json.Marshal(calls)
	if err != nil {
		return err
	}
	instancesJSON, err := json.Marshal(instances)
	if err != nil {
		return err
	}
	resourcesJSON, err := json.Marshal(resources)
	if err != nil {
		return err
	}
	data := map[string]string{
		"calls": string(callsJSON), "instances": string(instancesJSON), keyResources: string(resourcesJSON),
		"revision": strconv.FormatUint(revision, 10),
	}

	key := types.NamespacedName{Namespace: d.opts.ConfigNamespace, Name: d.opts.StateName}
	reader := d.cfg.Reader
	if reader == nil {
		reader = d.cfg.Client
	}
	cm := &corev1.ConfigMap{}
	err = reader.Get(ctx, key, cm)
	switch {
	case apierrors.IsNotFound(err):
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}, Data: data}
		err = d.cfg.Client.Create(ctx, cm)
	case err == nil:
		cm.Data = data
		err = d.cfg.Client.Update(ctx, cm)
	}
	if err != nil {
		d.mu.Lock()
		d.dirty = true
		d.mu.Unlock()
		return err
	}
	d.mu.Lock()
	d.published = max(d.published, revision)
	d.mu.Unlock()
	return nil
}

// recordResource records a call on a resource (ensure, delete).
func (d *Driver) recordResource(op string, r *resourceRecord, result string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, callRecord{
		Time: d.clock.Now().UTC(), Op: "resource-" + op, Region: r.Region, Result: result, Resource: r.ID, Kind: r.Kind,
	})
	if len(d.calls) > maxCalls {
		d.calls = slices.Clone(d.calls[len(d.calls)-maxCalls:])
	}
	d.markDirtyLocked()
}

// registerMetrics exposes the state revisions: scenarios read the current
// revision and wait until the published ConfigMap has reached it, instead of
// sleeping for the debounce.
func (d *Driver) registerMetrics() {
	for _, c := range []prometheus.Collector{
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "scarpenter_fake_state_revision",
			Help: "Revision of the fake driver's in-memory state (it grows with every change).",
		}, func() float64 { d.mu.Lock(); defer d.mu.Unlock(); return float64(d.revision) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "scarpenter_fake_state_published_revision",
			Help: "Revision of the fake driver's state ConfigMap.",
		}, func() float64 { d.mu.Lock(); defer d.mu.Unlock(); return float64(d.published) }),
	} {
		err := metrics.Registry.Register(c)
		if already, ok := errors.AsType[prometheus.AlreadyRegisteredError](err); ok {
			metrics.Registry.Unregister(already.ExistingCollector)
			_ = metrics.Registry.Register(c)
		}
	}
}

// CallRecord is one vendor call the driver recorded.
type CallRecord = callRecord

// Calls returns the recorded vendor calls of an operation (create or delete;
// "" for every operation), oldest first. Tests use it to assert which vendor
// calls were made.
func (d *Driver) Calls(op string) []CallRecord {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []CallRecord
	for _, c := range d.calls {
		if op == "" || c.Op == op {
			out = append(out, c)
		}
	}
	return out
}
