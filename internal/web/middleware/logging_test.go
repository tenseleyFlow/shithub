// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestStatusRecorder_UnwrapEnablesResponseController pins firedrill v4:
// statusRecorder MUST implement Unwrap so http.NewResponseController
// can reach the underlying conn. Pre-fix, the git smart-HTTP handler's
// per-request SetWriteDeadline silently returned ErrNotSupported and
// the http.Server's 30s WriteTimeout killed long pushes mid-stream.
func TestStatusRecorder_UnwrapEnablesResponseController(t *testing.T) {
	// fakeDeadlineRW is the underlying writer NewResponseController must
	// reach via Unwrap. It records that SetWriteDeadline was called with
	// the zero value (i.e. the caller is clearing the deadline).
	var sawClear bool
	inner := &fakeDeadlineRW{
		ResponseRecorder: httptest.NewRecorder(),
		onSetWrite: func(t time.Time) error {
			if t.IsZero() {
				sawClear = true
			}
			return nil
		},
	}

	rec := newStatusRecorder(inner)
	rc := http.NewResponseController(rec)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		if errors.Is(err, http.ErrNotSupported) {
			t.Fatalf("ResponseController.SetWriteDeadline returned ErrNotSupported: statusRecorder is missing Unwrap")
		}
		t.Fatalf("SetWriteDeadline: %v", err)
	}
	if !sawClear {
		t.Error("SetWriteDeadline did not reach the underlying writer")
	}
}

// fakeDeadlineRW lets the test observe what ResponseController calls
// hit the underlying writer. We embed an httptest.ResponseRecorder to
// satisfy http.ResponseWriter and add the deadline methods that
// NewResponseController probes for.
type fakeDeadlineRW struct {
	*httptest.ResponseRecorder
	onSetWrite func(time.Time) error
	onSetRead  func(time.Time) error
}

func (f *fakeDeadlineRW) SetWriteDeadline(t time.Time) error {
	if f.onSetWrite != nil {
		return f.onSetWrite(t)
	}
	return nil
}

func (f *fakeDeadlineRW) SetReadDeadline(t time.Time) error {
	if f.onSetRead != nil {
		return f.onSetRead(t)
	}
	return nil
}
