/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package tailnettest has in-memory fakes of the Tailscale API v2 and the Headscale 0.28+
// REST API as httptest servers, shaped after the real APIs (paths, bodies, status codes).
//
// They record calls, enforce Bearer authentication, tags, expiry and single use, and can
// inject 429, 5xx and slow answers. Join simulates a node registering with a key.
package tailnettest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Call is one request the fake received.
type Call struct {
	Method string
	Path   string
}

type fault struct {
	status     int
	retryAfter string
}

// server is what both fakes share: the listener, the call log, the clock and the fault
// injection. Handlers run under mu.
type server struct {
	srv *httptest.Server

	mu     sync.Mutex
	calls  []Call
	faults []fault
	delay  time.Duration
	now    func() time.Time
	seq    int
}

func newServer(t testing.TB, h http.Handler, s *server) {
	t.Helper()
	s.now = time.Now
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.calls = append(s.calls, Call{Method: r.Method, Path: r.URL.Path})
		delay := s.delay
		var f *fault
		if len(s.faults) > 0 {
			f, s.faults = &s.faults[0], s.faults[1:]
		}
		s.mu.Unlock()
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		if f != nil {
			if f.retryAfter != "" {
				w.Header().Set("Retry-After", f.retryAfter)
			}
			http.Error(w, `{"message":"injected fault"}`, f.status)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(s.srv.Close)
}

// URL is the fake's base URL.
func (s *server) URL() string { return s.srv.URL }

// Calls returns the requests received so far.
func (s *server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// CountCalls counts the requests with the method and path.
func (s *server) CountCalls(method, path string) int {
	n := 0
	for _, c := range s.Calls() {
		if c.Method == method && c.Path == path {
			n++
		}
	}
	return n
}

// FailNext answers the next n requests with the status (and a Retry-After header when
// retryAfter is not empty) without touching the fake's state.
func (s *server) FailNext(n, status int, retryAfter string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for range n {
		s.faults = append(s.faults, fault{status: status, retryAfter: retryAfter})
	}
}

// SetDelay makes every request wait before it is answered.
func (s *server) SetDelay(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delay = d
}

// SetNow replaces the fake's clock (key expiry).
func (s *server) SetNow(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// next returns the next sequence number; call with mu held.
func (s *server) next() int {
	s.seq++
	return s.seq
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func bearer(r *http.Request) string {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return tok
}

func validTag(t string) bool {
	return strings.HasPrefix(t, "tag:") && len(t) > 4 && t == strings.ToLower(t) && !strings.ContainsAny(t, " \t")
}

// JoinError is why a key was refused at registration.
type JoinError struct{ Reason string }

func (e *JoinError) Error() string { return "join refused: " + e.Reason }

func addr(n int) (v4, v6 string) {
	return fmt.Sprintf("100.64.%d.%d", n/250, n%250+1), fmt.Sprintf("fd7a:115c:a1e0::%x", n)
}
