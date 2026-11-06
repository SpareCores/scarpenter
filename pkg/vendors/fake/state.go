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
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
}

// markDirtyLocked schedules a debounced write of the state ConfigMap. The
// caller holds d.mu.
func (d *Driver) markDirtyLocked() {
	d.dirty = true
	if d.flushing || d.cfg.Client == nil {
		return
	}
	d.flushing = true
	interval := d.opts.StateInterval
	if interval <= 0 {
		interval = defaultStateInterval
	}
	time.AfterFunc(interval, func() {
		d.mu.Lock()
		d.flushing = false
		d.mu.Unlock()
		if err := d.Flush(context.Background()); err != nil {
			d.cfg.Log.Error(err, "writing the fake driver's state ConfigMap")
		}
	})
}

// Flush writes the state ConfigMap now if anything changed since the last
// write.
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
	calls := slices.Clone(d.calls)
	instances := make([]instanceRecord, 0, len(d.instances))
	for _, i := range d.instances {
		instances = append(instances, instanceRecord{
			Ref: string(i.Ref), Region: i.Region, Zone: i.Zone, InstanceType: i.Type, CapacityType: i.CapacityType,
			State: i.State, NodeClaim: i.claim, CreatedAt: i.CreatedAt,
		})
	}
	d.mu.Unlock()
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
	data := map[string]string{"calls": string(callsJSON), "instances": string(instancesJSON)}

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
	}
	return err
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
