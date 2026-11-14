/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/oracle/oci-go-sdk/v65/identity"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// These tests run the real SDK adapter against a local server: the retry policy
// (the launch is never retried, the reads are), the per-call deadline, the user
// agent and the per-region clients. No credential is real: the key is made here.

func testProvider(t *testing.T) common.ConfigurationProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return common.NewRawConfigurationProvider("ocid1.tenancy.oc1..aaaa", "ocid1.user.oc1..aaaa", "eu-frankfurt-1",
		"aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99", string(pemKey), nil)
}

// server counts the requests and answers by the handler.
type server struct {
	*httptest.Server
	requests atomic.Int32
	mu       sync.Mutex
	agents   []string
	tokens   []string
	hosts    []string
}

func newServer(t *testing.T, h func(n int, w http.ResponseWriter, r *http.Request)) (*server, *SDK) {
	t.Helper()
	s := &server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(s.requests.Add(1))
		s.mu.Lock()
		s.agents = append(s.agents, r.Header.Get("User-Agent"))
		s.tokens = append(s.tokens, r.Header.Get("opc-retry-token"))
		s.mu.Unlock()
		h(n, w, r)
	}))
	t.Cleanup(s.Close)
	sdk := NewSDK(testProvider(t), "scarpenter/test", WithSDKTimeout(10*time.Second))
	sdk.host = func(service, region string) string {
		s.mu.Lock()
		s.hosts = append(s.hosts, service+"/"+region)
		s.mu.Unlock()
		return s.URL
	}
	return s, sdk
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

const capacityBody = `{"code":"InternalError","message":"Out of host capacity."}`

func launchRequest(token string) core.LaunchInstanceRequest {
	return core.LaunchInstanceRequest{
		LaunchInstanceDetails: core.LaunchInstanceDetails{
			CompartmentId: new("ocid1.compartment.oc1..aaaa"), AvailabilityDomain: new("x:AD-1"),
			Shape: new("VM.Standard.E5.Flex"),
		},
		OpcRetryToken: new(token),
	}
}

// The launch is never retried, whatever policy the request carries: an HTTP 500 "Out of
// host capacity" must reach the driver at once (plan 25, finding 7).
func TestSDKLaunchIsNotRetried(t *testing.T) {
	for name, withPolicy := range map[string]bool{"no policy on the request": false, "the default policy on the request": true} {
		t.Run(name, func(t *testing.T) {
			s, sdk := newServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { writeJSON(w, 500, capacityBody) })
			req := launchRequest("scarpenter-token")
			if withPolicy {
				p := common.DefaultRetryPolicy()
				req.RequestMetadata.RetryPolicy = &p
			}
			_, err := sdk.LaunchInstance(t.Context(), "eu-frankfurt-1", req)
			if err == nil {
				t.Fatal("no error")
			}
			if n := s.requests.Load(); n != 1 {
				t.Errorf("the launch was sent %d times; a 500 must not be retried", n)
			}
			if got := vendors.ClassOf(classify(err, kindLaunch)); got != vendors.InsufficientCapacity {
				t.Errorf("the error is classified %q: %v", got, err)
			}
			if s.tokens[0] != "scarpenter-token" {
				t.Errorf("the retry token on the wire is %q, not the driver's", s.tokens[0])
			}
		})
	}
}

// The reads retry a 5xx with backoff, the 404 is not retried (the default policy with
// eventual consistency would hide NotFound for minutes), and the user agent is ours.
func TestSDKReadsRetryAndNotFoundDoesNot(t *testing.T) {
	t.Run("a 500 is retried", func(t *testing.T) {
		s, sdk := newServer(t, func(n int, w http.ResponseWriter, _ *http.Request) {
			if n == 1 {
				writeJSON(w, 500, `{"code":"InternalError","message":"try again"}`)
				return
			}
			writeJSON(w, 200, `[]`)
		})
		if _, err := sdk.ListInstances(t.Context(), "eu-frankfurt-1", core.ListInstancesRequest{
			CompartmentId: new("ocid1.compartment.oc1..aaaa")}); err != nil {
			t.Fatal(err)
		}
		if n := s.requests.Load(); n != 2 {
			t.Errorf("%d requests, want the failed one and its retry", n)
		}
		for _, a := range s.agents {
			if !strings.HasPrefix(a, "scarpenter/test") {
				t.Errorf("user agent %q", a)
			}
		}
	})
	t.Run("a 404 is not", func(t *testing.T) {
		s, sdk := newServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, 404, `{"code":"NotAuthorizedOrNotFound","message":"gone"}`)
		})
		_, err := sdk.GetInstance(t.Context(), "eu-frankfurt-1", core.GetInstanceRequest{
			InstanceId: new("ocid1.instance.oc1.eu-frankfurt-1.aaaa")})
		if !vendors.IsClass(classify(err, kindRead), vendors.NotFound) {
			t.Errorf("error %v, want NotFound", err)
		}
		if n := s.requests.Load(); n != 1 {
			t.Errorf("%d requests for a 404; it must not be retried", n)
		}
	})
}

// Every call has a deadline: a server that never answers does not hold the driver.
func TestSDKCallDeadline(t *testing.T) {
	if raceEnabled {
		// common.Retry of oci-go-sdk v65.130.0 returns its goroutine's response variable
		// when the context ends, without synchronisation: a data race inside the SDK
		// whenever a call times out. TestWithinDeadline covers the deadline itself.
		t.Skip("the SDK's retry loop races on a deadline (oci-go-sdk v65.130.0, common/retry.go)")
	}
	release := make(chan struct{})
	_, sdk := newServer(t, func(_ int, _ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	t.Cleanup(func() { close(release) })
	sdk.timeout = 200 * time.Millisecond
	start := time.Now()
	_, err := sdk.GetInstance(t.Context(), "eu-frankfurt-1", core.GetInstanceRequest{
		InstanceId: new("ocid1.instance.oc1.eu-frankfurt-1.aaaa")})
	if err == nil {
		t.Fatal("no error")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the call took %s with a deadline of 200ms", took)
	}
	cerr := classify(err, kindRead)
	if !vendors.IsClass(cerr, vendors.Retryable) || vendors.CodeOf(cerr) != codeTimeout {
		t.Errorf("a timeout is %v (%s %s), want Retryable %s", cerr, vendors.ClassOf(cerr), vendors.CodeOf(cerr), codeTimeout)
	}
}

// The clients are made per region and per service, the identity calls of the
// subscriptions go to the home region, and the launch carries the deadline too.
func TestSDKRegionsAndClients(t *testing.T) {
	s, sdk := newServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, `[]`) })
	ctx := t.Context()
	for _, region := range []string{"eu-frankfurt-1", "us-ashburn-1", "eu-frankfurt-1"} {
		if _, err := sdk.ListInstances(ctx, region, core.ListInstancesRequest{
			CompartmentId: new("ocid1.compartment.oc1..aaaa")}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sdk.ListRegionSubscriptions(ctx, identity.ListRegionSubscriptionsRequest{
		TenancyId: new("ocid1.tenancy.oc1..aaaa")}); err != nil {
		t.Fatal(err)
	}
	if _, err := sdk.ListAvailabilityDomains(ctx, "us-ashburn-1", identity.ListAvailabilityDomainsRequest{
		CompartmentId: new("ocid1.tenancy.oc1..aaaa")}); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"compute/eu-frankfurt-1": true, "compute/us-ashburn-1": true,
		"identity/eu-frankfurt-1": true, "identity/us-ashburn-1": true}
	if len(s.hosts) != len(want) {
		t.Errorf("clients made %v; one per service and region", s.hosts)
	}
	for _, h := range s.hosts {
		if !want[h] {
			t.Errorf("unexpected client %s", h)
		}
	}
	if got := NewSDK(testProvider(t), "x", WithSDKTimeout(0)).timeout; got != DefaultCallTimeout {
		t.Errorf("a zero timeout gives %s, want the default %s", got, DefaultCallTimeout)
	}
}

// within puts the deadline on the call and a shorter one of the caller wins; this is the
// part of the adapter's deadline that does not go through the SDK's retry loop.
func TestWithinDeadline(t *testing.T) {
	block := func(ctx context.Context, _ int) (int, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	start := time.Now()
	_, err := within(t.Context(), 50*time.Millisecond, block, 0)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Errorf("within = %v after %s, want the deadline", err, time.Since(start))
	}
	if got := vendors.ClassOf(classify(err, kindRead)); got != vendors.Retryable {
		t.Errorf("the deadline is classified %q", got)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	start = time.Now()
	if _, err := within(ctx, time.Hour, block, 0); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Errorf("the caller's shorter deadline did not win: %v after %s", err, time.Since(start))
	}
}
