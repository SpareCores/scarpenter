/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package tailnet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	defaultCallTimeout = 15 * time.Second
	defaultAttempts    = 4
	defaultBackoff     = 500 * time.Millisecond
	// A 429 whose Retry-After is longer than this is returned to the caller (who requeues)
	// instead of being waited out inside the call.
	maxInlineWait     = 5 * time.Second
	defaultRetryAfter = 10 * time.Second
	maxResponseBytes  = 8 << 20
	maxErrorText      = 200
	limiterRate       = 10
	limiterBurst      = 20
)

// Config is what both backends share. Zero values are the production defaults.
type Config struct {
	// BaseURL is the API's base URL (scheme, host and port; no /api path).
	BaseURL string
	// HTTPClient defaults to a client with no timeout of its own: every attempt gets
	// CallTimeout through its context.
	HTTPClient *http.Client
	// CallTimeout bounds one HTTP attempt (default 15 s).
	CallTimeout time.Duration
	// MaxAttempts bounds the attempts of one call for 429, 5xx and transport errors (default 4).
	MaxAttempts int
	// Backoff is the first retry delay; it doubles with jitter (default 500 ms).
	Backoff time.Duration
	// Now and Sleep are for tests.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

// client sends requests with a deadline per attempt, a client-side rate limit, retries
// for 429, 5xx and transport errors, and a gate that fails calls fast after a 429.
type client struct {
	base     *url.URL
	hc       *http.Client
	timeout  time.Duration
	attempts int
	backoff  time.Duration
	now      func() time.Time
	sleep    func(ctx context.Context, d time.Duration) error
	lim      *rate.Limiter

	mu           sync.Mutex
	blockedUntil time.Time
}

func newClient(cfg Config) (*client, error) {
	u, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("tailnet API URL %q must be an http(s) URL with a host", cfg.BaseURL)
	}
	c := &client{
		base:     u,
		hc:       cfg.HTTPClient,
		timeout:  cfg.CallTimeout,
		attempts: cfg.MaxAttempts,
		backoff:  cfg.Backoff,
		now:      cfg.Now,
		sleep:    cfg.Sleep,
		lim:      rate.NewLimiter(limiterRate, limiterBurst),
	}
	if c.hc == nil {
		c.hc = &http.Client{}
	}
	if c.timeout <= 0 {
		c.timeout = defaultCallTimeout
	}
	if c.attempts <= 0 {
		c.attempts = defaultAttempts
	}
	if c.backoff <= 0 {
		c.backoff = defaultBackoff
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.sleep == nil {
		c.sleep = sleepContext
	}
	return c, nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// call describes one API call. Authorize sets the credentials on each attempt; Reauth, when
// set, is told about a 401 so the next attempt can fetch fresh credentials (one extra
// attempt per call).
type call struct {
	op        string
	method    string
	path      string
	form      url.Values // sent as a form body when set
	body      any        // sent as JSON when set
	authorize func(ctx context.Context, r *http.Request) error
	reauth    func()
	// notFoundOK makes a 404 a success (idempotent revoke and delete).
	notFoundOK bool
}

// do runs the call and decodes a JSON response into out (nil: ignore the body).
func (c *client) do(ctx context.Context, cl call, out any) error {
	if err := c.gate(cl.op); err != nil {
		return err
	}
	reauthed := false
	for attempt := 1; ; attempt++ {
		err := c.once(ctx, cl, out)
		if err == nil {
			return nil
		}
		e, _ := errors.AsType[*Error](err)
		if e == nil {
			return err
		}
		if e.Class == NotFound && cl.notFoundOK {
			return nil
		}
		if e.Status == http.StatusUnauthorized && cl.reauth != nil && !reauthed {
			reauthed = true
			cl.reauth()
			attempt--
			continue
		}
		if e.Status == http.StatusTooManyRequests {
			c.block(e.RetryAfter)
		}
		if e.Class != Retryable || attempt >= c.attempts || ctx.Err() != nil {
			return err
		}
		wait := c.retryDelay(attempt, e.RetryAfter)
		if e.RetryAfter > maxInlineWait {
			return err
		}
		if serr := c.sleep(ctx, wait); serr != nil {
			return &Error{Class: Retryable, Op: cl.op, Err: serr}
		}
	}
}

func (c *client) retryDelay(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	d := c.backoff << (attempt - 1)
	return d/2 + rand.N(d/2+1) //nolint:gosec // jitter, not security
}

// gate fails a call fast while a 429's Retry-After has not passed.
func (c *client) gate(op string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if wait := c.blockedUntil.Sub(c.now()); wait > 0 {
		return &Error{
			Class: Retryable, Op: op, Status: http.StatusTooManyRequests,
			Message: "rate limited earlier; not calling the API yet", RetryAfter: wait,
		}
	}
	return nil
}

func (c *client) block(retryAfter time.Duration) {
	if retryAfter <= 0 {
		retryAfter = defaultRetryAfter
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if until := c.now().Add(retryAfter); until.After(c.blockedUntil) {
		c.blockedUntil = until
	}
}

func (c *client) once(ctx context.Context, cl call, out any) error {
	if err := c.lim.Wait(ctx); err != nil {
		return &Error{Class: Retryable, Op: cl.op, Err: err}
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var body io.Reader
	contentType := ""
	switch {
	case cl.form != nil:
		body = strings.NewReader(cl.form.Encode())
		contentType = "application/x-www-form-urlencoded"
	case cl.body != nil:
		b, err := json.Marshal(cl.body)
		if err != nil {
			return &Error{Class: Fatal, Op: cl.op, Message: "encoding the request failed"}
		}
		body = bytes.NewReader(b)
		contentType = "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, cl.method, c.base.JoinPath(cl.path).String(), body)
	if err != nil {
		return &Error{Class: Fatal, Op: cl.op, Message: "building the request failed"}
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if cl.authorize != nil {
		if err := cl.authorize(ctx, req); err != nil {
			return err
		}
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		// The *url.Error text holds the URL but never a header or a body.
		return &Error{Class: Retryable, Op: cl.op, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return &Error{Class: Retryable, Op: cl.op, Status: resp.StatusCode, Message: "reading the response failed"}
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &Error{
			Class: classify(resp.StatusCode), Op: cl.op, Status: resp.StatusCode,
			Message: apiErrorText(data, resp.StatusCode), RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), c.now()),
		}
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		// Not wrapped: the decoder's message can quote the document, which may hold a key.
		return &Error{Class: Fatal, Op: cl.op, Status: resp.StatusCode, Message: "malformed response"}
	}
	return nil
}

// apiErrorText extracts the API's error text from a JSON error document ("message" for the
// Tailscale API and Headscale's gateway, "error_description" and "error" for OAuth).
// Anything else yields the status text: an unknown body is never copied into an error.
func apiErrorText(data []byte, status int) string {
	var doc struct {
		Message          string `json:"message"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if json.Unmarshal(data, &doc) == nil {
		for _, s := range []string{doc.Message, doc.ErrorDescription, doc.Error} {
			if s = strings.TrimSpace(s); s != "" {
				return truncate(s, maxErrorText)
			}
		}
	}
	return http.StatusText(status)
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return max(time.Duration(secs)*time.Second, 0)
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(t.Sub(now), 0)
	}
	return 0
}
