/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp/fake"
)

// TestTimeoutsBoundClientRetries: the generated client retries a read on 503
// until its context ends (600 s without a deadline). Under WithTimeouts the call
// gives up at the call deadline with a Retryable CallTimeout.
func TestTimeoutsBoundClientRetries(t *testing.T) {
	var n atomic.Int32
	s, _ := sdkServer(t, func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprint(w, `{"error":{"code":503,"message":"down","errors":[{"reason":"backendError"}]}}`)
	})
	api := WithTimeouts(s, 500*time.Millisecond, 0)
	start := time.Now()
	_, err := api.GetInstance(t.Context(), &computepb.GetInstanceRequest{Project: "p", Zone: "z-a", Instance: "i"})
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the call took %s: not bounded by the call deadline", elapsed)
	}
	err = classify(err, errContext{})
	if vendors.ClassOf(err) != vendors.Retryable || vendors.CodeOf(err) != codeTimeout {
		t.Fatalf("err = %v (class %s, code %s), want Retryable %s", err, vendors.ClassOf(err), vendors.CodeOf(err), codeTimeout)
	}
	if n.Load() < 2 {
		t.Fatalf("requests = %d: the client was expected to retry the 503", n.Load())
	}
}

// TestTimeoutsNoRetryOn429: the generated client does not retry 429 (only 503
// and 504 on reads); the error comes back at once and is Retryable.
func TestTimeoutsNoRetryOn429(t *testing.T) {
	var n atomic.Int32
	s, _ := sdkServer(t, func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprint(w, `{"error":{"code":429,"message":"slow","errors":[{"reason":"rateLimitExceeded"}]}}`)
	})
	_, err := WithTimeouts(s, 5*time.Second, 0).
		GetInstance(t.Context(), &computepb.GetInstanceRequest{Project: "p", Zone: "z-a", Instance: "i"})
	if vendors.ClassOf(classify(err, errContext{})) != vendors.Retryable || n.Load() != 1 {
		t.Fatalf("err = %v after %d requests", err, n.Load())
	}
}

// blockingAPI blocks every call until its context ends and records the
// deadline it saw.
type blockingAPI struct {
	ComputeAPI
	left atomic.Int64
}

func (b *blockingAPI) block(ctx context.Context) error {
	if dl, ok := ctx.Deadline(); ok {
		b.left.Store(int64(time.Until(dl)))
	}
	<-ctx.Done()
	return ctx.Err()
}

func (b *blockingAPI) InsertInstance(ctx context.Context, _ *computepb.InsertInstanceRequest) (*computepb.Operation, error) {
	return nil, b.block(ctx)
}

func (b *blockingAPI) WaitZoneOperation(
	ctx context.Context, _ *computepb.WaitZoneOperationRequest,
) (*computepb.Operation, error) {
	return nil, b.block(ctx)
}

func TestTimeoutsPerKind(t *testing.T) {
	b := &blockingAPI{}
	api := WithTimeouts(b, 50*time.Millisecond, 80*time.Millisecond)

	_, err := api.InsertInstance(t.Context(), &computepb.InsertInstanceRequest{})
	if vendors.CodeOf(classify(err, errContext{})) != codeTimeout || time.Duration(b.left.Load()) > 50*time.Millisecond {
		t.Fatalf("insert: err = %v, deadline in %s", err, time.Duration(b.left.Load()))
	}
	_, err = api.WaitZoneOperation(t.Context(), &computepb.WaitZoneOperationRequest{})
	left := time.Duration(b.left.Load())
	if vendors.CodeOf(classify(err, errContext{})) != codeTimeout || left <= 50*time.Millisecond || left > 80*time.Millisecond {
		t.Fatalf("wait: err = %v, deadline in %s", err, left)
	}

	// A shorter deadline of the caller wins.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	_, _ = api.WaitZoneOperation(ctx, &computepb.WaitZoneOperationRequest{})
	if left := time.Duration(b.left.Load()); left > 10*time.Millisecond {
		t.Fatalf("caller deadline lost: %s", left)
	}

	// Zero durations select the defaults.
	d := WithTimeouts(b, 0, 0).(*timedCompute)
	if d.call != DefaultCallTimeout || d.wait != DefaultWaitTimeout {
		t.Fatalf("defaults = %s, %s", d.call, d.wait)
	}
}

// TestTimeoutsForwardEveryMethod calls every ComputeAPI method through the
// wrapper and checks that it reaches the wrapped API under a deadline.
func TestTimeoutsForwardEveryMethod(t *testing.T) {
	f := fake.New("p")
	api := WithTimeouts(f, time.Second, 2*time.Second)
	v := reflect.ValueOf(api)
	it := reflect.TypeFor[ComputeAPI]()
	for meth := range it.Methods() {
		name := meth.Name
		m := v.MethodByName(name)
		req := reflect.New(m.Type().In(1).Elem())
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s panicked: %v", name, r)
				}
			}()
			m.Call([]reflect.Value{reflect.ValueOf(t.Context()), req})
		}()
		if f.Calls(name) != 1 {
			t.Errorf("%s: %d calls reached the API", name, f.Calls(name))
		}
	}
}
