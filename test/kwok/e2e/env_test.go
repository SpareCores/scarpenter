//go:build kwok

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package e2e holds the scenarios of the L3 lane (docs/plans/v1/wip/
// 00-overview.md section 13.2). test/kwok/run.sh builds the cluster they run in:
// SCarpenter (real Karpenter core plus the fake driver), kwok-controller and a
// Keeper replay server. The scenarios steer the fake driver through its
// ConfigMaps and read what it did from its state ConfigMap.
package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/SpareCores/sc-keeper-go/keepertest"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"

	// Registers Karpenter core's API types in client-go's scheme.
	_ "sigs.k8s.io/karpenter/pkg/apis/v1"
)

const (
	ns          = "scarpenter-system"
	fakeConfig  = "scarpenter-fake"
	fakeState   = "scarpenter-fake-state"
	vendor      = "aws"
	controllerL = "app=scarpenter"
)

var (
	k8s       client.Client
	clientset kubernetes.Interface
	outDir    string
	// replay serves the snapshot to the in-process reference catalogs.
	replay *httptest.Server
)

func TestMain(m *testing.M) {
	if err := v1alpha1.AddToScheme(scheme.Scheme); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cfg, err := ctrl.GetConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubeconfig:", err)
		os.Exit(1)
	}
	cfg.QPS, cfg.Burst = 100, 200
	if k8s, err = client.New(cfg, client.Options{Scheme: scheme.Scheme}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if clientset, err = kubernetes.NewForConfig(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if outDir = os.Getenv("KWOK_OUT_DIR"); outDir == "" {
		outDir = os.TempDir()
	}
	h, err := keepertest.NewHandler(catalogtest.SnapshotDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	replay = httptest.NewServer(h)
	code := m.Run()
	replay.Close()
	os.Exit(code)
}

func ctx() context.Context { return context.Background() }

// eventually polls cond until it returns true; on timeout the test fails with
// the last explanation cond gave. It returns the time it waited.
func eventually(t *testing.T, timeout, interval time.Duration, what string, cond func() (bool, string)) time.Duration {
	t.Helper()
	start := time.Now()
	last := ""
	for {
		ok, why := cond()
		if ok {
			d := time.Since(start)
			t.Logf("%s (%s)", what, d.Round(100*time.Millisecond))
			return d
		}
		last = why
		if time.Since(start) > timeout {
			t.Fatalf("timed out after %s waiting for: %s; last state: %s", timeout, what, last)
		}
		time.Sleep(interval)
	}
}

// consistently fails the test when cond holds at any poll during d.
func consistently(t *testing.T, d, interval time.Duration, what string, bad func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if isBad, why := bad(); isBad {
			t.Fatalf("%s: %s", what, why)
		}
		time.Sleep(interval)
	}
}

// ---------------------------------------------------------------- fake ----

var steer int

// setFake writes keys of the fake driver's configuration ConfigMap and removes
// them again when the test ends. Every write is made unique with a comment, so
// that an identical rule written twice is a new rule (the fake resets the count
// of a rule only when the text changes).
func setFake(t *testing.T, kv map[string]string) {
	t.Helper()
	steer++
	cm := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: ns, Name: fakeConfig}
	err := k8s.Get(ctx(), key, cm)
	create := apierrors.IsNotFound(err)
	if err != nil && !create {
		t.Fatal(err)
	}
	if create {
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: fakeConfig}}
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	for k, v := range kv {
		cm.Data[k] = fmt.Sprintf("# steering write %d\n%s", steer, v)
	}
	if create {
		err = k8s.Create(ctx(), cm)
	} else {
		err = k8s.Update(ctx(), cm)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clearFake(t, keys(kv)...) })
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func clearFake(t *testing.T, ks ...string) {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := k8s.Get(ctx(), client.ObjectKey{Namespace: ns, Name: fakeConfig}, cm); err != nil {
		return
	}
	for _, k := range ks {
		delete(cm.Data, k)
	}
	if err := k8s.Update(ctx(), cm); err != nil {
		t.Logf("clearing the fake configuration: %v", err)
	}
}

// call and instance are the records of the fake's state ConfigMap.
type call struct {
	Time         time.Time `json:"time"`
	Op           string    `json:"op"`
	Region       string    `json:"region"`
	Zone         string    `json:"zone"`
	InstanceType string    `json:"instanceType"`
	CapacityType string    `json:"capacityType"`
	NodeClaim    string    `json:"nodeClaim"`
	Result       string    `json:"result"`
}

type instance struct {
	Ref          string    `json:"ref"`
	Region       string    `json:"region"`
	Zone         string    `json:"zone"`
	InstanceType string    `json:"instanceType"`
	CapacityType string    `json:"capacityType"`
	State        string    `json:"state"`
	NodeClaim    string    `json:"nodeClaim"`
	CreatedAt    time.Time `json:"createdAt"`
	// UserDataBytes is the size of the user-data the fake received.
	UserDataBytes int `json:"userDataBytes"`
}

func readState(t *testing.T) ([]call, []instance) {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := k8s.Get(ctx(), client.ObjectKey{Namespace: ns, Name: fakeState}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		t.Fatal(err)
	}
	var calls []call
	var insts []instance
	if err := json.Unmarshal([]byte(cm.Data["calls"]), &calls); err != nil && cm.Data["calls"] != "" {
		t.Fatalf("calls: %v", err)
	}
	if err := json.Unmarshal([]byte(cm.Data["instances"]), &insts); err != nil && cm.Data["instances"] != "" {
		t.Fatalf("instances: %v", err)
	}
	return calls, insts
}

// callsSince returns the calls made after the mark, optionally of one op.
func callsSince(t *testing.T, since time.Time, op string) []call {
	t.Helper()
	// The fake publishes its state debounced (500 ms): let a call that just
	// happened show up.
	time.Sleep(1200 * time.Millisecond)
	calls, _ := readState(t)
	var out []call
	for _, c := range calls {
		if c.Time.After(since) && (op == "" || c.Op == op) {
			out = append(out, c)
		}
	}
	return out
}

// liveInstances returns the instances that are not terminated.
func liveInstances(t *testing.T) []instance {
	t.Helper()
	time.Sleep(1200 * time.Millisecond) // the fake publishes its state debounced
	_, insts := readState(t)
	var out []instance
	for _, i := range insts {
		if i.State != "terminated" {
			out = append(out, i)
		}
	}
	return out
}

// ------------------------------------------------------------- metrics ----

// metrics scrapes the controller's /metrics through the API server's pod proxy
// and returns the value of every sample, keyed by the metric line up to the
// value (name and labels).
func metrics(t *testing.T) map[string]float64 {
	t.Helper()
	pods, err := clientset.CoreV1().Pods(ns).List(ctx(), metav1.ListOptions{LabelSelector: controllerL})
	if err != nil || len(pods.Items) == 0 {
		t.Fatalf("finding the controller pod: %v", err)
	}
	raw, err := clientset.CoreV1().Pods(ns).ProxyGet("http", pods.Items[0].Name, "8080", "metrics", nil).DoRaw(ctx())
	if err != nil {
		t.Logf("scraping the controller metrics: %v", err)
		return nil
	}
	out := map[string]float64{}
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		if i < 0 {
			continue
		}
		if v, err := strconv.ParseFloat(line[i+1:], 64); err == nil {
			out[line[:i]] = v
		}
	}
	return out
}

// appendOut appends a line to a file of the lane's output directory.
var outMu sync.Mutex

func appendOut(t *testing.T, file, line string) {
	t.Helper()
	outMu.Lock()
	defer outMu.Unlock()
	f, err := os.OpenFile(filepath.Join(outDir, file), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // test output
	if err != nil {
		t.Logf("writing %s: %v", file, err)
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintln(f, line)
}
