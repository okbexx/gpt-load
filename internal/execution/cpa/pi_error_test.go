package cpa

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gpt-load/internal/execution"
	"gpt-load/internal/subscription/providers/codex"
)

// Exercise the real Pi frame reader and adapter; PiError fields are private.
func TestPiAdapterErrorEvidence(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name, frames, code string
			status             int
			dispatch           execution.DispatchState
			kind               execution.ErrorKind
			hint               execution.FailureHint
			retry              time.Duration
		}{
			{"rate_limit", `{"type":"error","code":"upstream_error","status":429,"dispatch_state":"maybe_sent","retry_after_seconds":17}`, "pi_upstream_error", 429, execution.DispatchMaybeSent, execution.ErrorKindHTTP, execution.FailureHintRateLimited, 17 * time.Second},
			{"host_error", `{"type":"error","code":"upstream_error","status":503,"dispatch_state":"maybe_sent","retry_after_seconds":3}`, "pi_upstream_error", 503, execution.DispatchMaybeSent, execution.ErrorKindHTTP, execution.FailureHintHostError, 3 * time.Second},
			{"unauthorized_upstream", `{"type":"error","code":"unauthorized","status":401,"dispatch_state":"maybe_sent"}`, "pi_unauthorized", 401, execution.DispatchMaybeSent, execution.ErrorKindHTTP, execution.FailureHintRefreshRequired, 0},
			{"unauthorized_unknown_code", `{"type":"error","code":"SYNTHETIC_PRIVATE_DETAIL","status":401,"dispatch_state":"maybe_sent"}`, "pi_bridge_error", 401, execution.DispatchMaybeSent, execution.ErrorKindHTTP, execution.FailureHintRefreshRequired, 0},
			{"unauthorized_not_sent", `{"type":"error","code":"unauthorized","status":401,"dispatch_state":"not_sent"}`, "pi_unauthorized", 0, execution.DispatchNotSent, execution.ErrorKindInternal, "", 0},
			{"not_sent", `{"type":"error","code":"unsupported_capability","status":0,"dispatch_state":"not_sent"}`, "pi_unsupported_capability", 0, execution.DispatchNotSent, execution.ErrorKindConversionUnsupported, "", 0},
			{"failed_after_headers", piTestHeaders + "\n" + `{"type":"error","code":"upstream_error","status":200,"dispatch_state":"maybe_sent"}`, "pi_upstream_error", 0, execution.DispatchMaybeSent, execution.ErrorKindTransport, "", 0},
			{"rate_limit_after_headers", piTestHeaders + "\n" + `{"type":"error","code":"upstream_error","status":429,"dispatch_state":"maybe_sent","retry_after_seconds":17}`, "pi_upstream_error", 429, execution.DispatchMaybeSent, execution.ErrorKindHTTP, execution.FailureHintRateLimited, 17 * time.Second},
			{"invalid_retry", `{"type":"error","code":"upstream_error","status":429,"dispatch_state":"maybe_sent","retry_after_seconds":-1}`, "pi_upstream_error", 429, execution.DispatchMaybeSent, execution.ErrorKindHTTP, execution.FailureHintRateLimited, 0},
			{"actual_http_error", `{"type":"error","code":"upstream_http_error","status":429,"dispatch_state":"maybe_sent","retry_after_seconds":17}`, "pi_upstream_http_error", 429, execution.DispatchMaybeSent, execution.ErrorKindHTTP, execution.FailureHintRateLimited, 17 * time.Second},
			{"sidecar_timeout_before_dispatch", `{"type":"error","code":"timeout","status":408,"dispatch_state":"not_sent"}`, "pi_timeout", 0, execution.DispatchNotSent, execution.ErrorKindTimeout, "", 0},
			{"sidecar_cancel_before_dispatch", `{"type":"error","code":"cancelled","status":0,"dispatch_state":"not_sent"}`, "pi_cancelled", 0, execution.DispatchNotSent, execution.ErrorKindCanceled, "", 0},
			{"sidecar_timeout_after_headers", piTestHeaders + "\n" + `{"type":"error","code":"timeout","status":200,"dispatch_state":"maybe_sent"}`, "pi_timeout", 0, execution.DispatchMaybeSent, execution.ErrorKindTimeout, "", 0},
			{"unknown_code_redacted", `{"type":"error","code":"SYNTHETIC_PRIVATE_DETAIL","status":503,"dispatch_state":"maybe_sent"}`, "pi_bridge_error", 503, execution.DispatchMaybeSent, execution.ErrorKindHTTP, execution.FailureHintHostError, 0},
			{"missing_terminal", piTestHeaders, "pi_bridge_protocol_error", 0, execution.DispatchMaybeSent, execution.ErrorKindTransport, "", 0},
			{"bridge_cancelled", piTestHeaders + "\n" + `{"type":"error","code":"cancelled","status":200,"dispatch_state":"maybe_sent"}`, "pi_cancelled", 0, execution.DispatchMaybeSent, execution.ErrorKindCanceled, "", 0},
		} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, tc.frames) }))
				defer server.Close()
				a, spec := piErrorFixture(t, server.URL)
				var got execution.AttemptResult
				if stream {
					result := a.ExecuteStream(t.Context(), spec, func(execution.StreamEvent) error { return nil })
					got = execution.AttemptResult{DispatchState: result.DispatchState, ResponseStarted: result.StatusCode != 0, StatusCode: result.StatusCode, Error: result.Error}
				} else {
					got = a.Execute(t.Context(), spec)
				}
				if got.DispatchState != tc.dispatch || got.StatusCode != tc.status {
					t.Errorf("dispatch/status = %s/%d, want %s/%d", got.DispatchState, got.StatusCode, tc.dispatch, tc.status)
				}
				if err := got.Validate(); err != nil {
					t.Fatalf("invalid AttemptResult contract: %v; result=%+v", err, got)
				}
				e := got.Error
				if e == nil {
					t.Fatal("lost error evidence")
				}
				wantReplay := execution.ReplaySafetyUnknown
				if tc.status == http.StatusUnauthorized && tc.dispatch == execution.DispatchMaybeSent {
					wantReplay = execution.ReplaySafetyRejectedBeforeProcessing
				}
				if e.Kind != tc.kind || e.StatusCode != tc.status || e.Type != "pi_driver_error" || e.Code != tc.code || e.Hint != tc.hint || e.RetryAfter != tc.retry || e.ScopeHint != "" || e.ReplaySafety != wantReplay {
					t.Errorf("evidence = %+v; want kind=%s status=%d type=pi_driver_error code=%s hint=%s retry=%s replay=%s, no inferred scope", e, tc.kind, tc.status, tc.code, tc.hint, tc.retry, wantReplay)
				}
				if tc.status == http.StatusUnauthorized && (e.OriginHint != execution.ErrorOriginUpstream || e.Hint != execution.FailureHintRefreshRequired) {
					t.Errorf("unauthorized evidence = %+v; want upstream refresh classification", e)
				}
			})
		}
	}
}

const piTestHeaders = `{"type":"headers","driver":"pi","status":200,"dispatch_state":"maybe_sent"}`

func piErrorFixture(t *testing.T, endpoint string) (*Adapter, execution.AttemptSpec) {
	t.Helper()
	a, _, _, keys, row := newAdapterFixture(t, credentialJSON("test-access", "test-refresh", time.Now().Add(time.Hour)))
	executor, err := codex.NewPiExecutor(endpoint, "test-bridge-secret-at-least-32-characters")
	if err != nil {
		t.Fatal(err)
	}
	a.piExecutor = executor
	spec := validSpec(t, row, keys)
	spec.TargetConfig = json.RawMessage(`{"execution_driver":"pi-experimental"}`)
	return a, spec
}

func TestPiAdapterContextErrorEvidence(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
			t.Run(fmt.Sprintf("%v/stream=%t", cause, stream), func(t *testing.T) {
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(context.Canceled)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprintln(w, piTestHeaders)
					w.(http.Flusher).Flush()
					cancel(cause)
					<-r.Context().Done()
				}))
				defer server.Close()
				a, spec := piErrorFixture(t, server.URL)
				var e *execution.ErrorEvidence
				var status int
				if stream {
					r := a.ExecuteStream(ctx, spec, func(execution.StreamEvent) error { return nil })
					e, status = r.Error, r.StatusCode
				} else {
					r := a.Execute(ctx, spec)
					e, status = r.Error, r.StatusCode
				}
				kind := execution.ErrorKindCanceled
				if cause == context.DeadlineExceeded {
					kind = execution.ErrorKindTimeout
				}
				if e == nil || e.Kind != kind || e.Hint == execution.FailureHintRefreshRequired || status != 0 {
					t.Fatalf("status=%d evidence=%+v, want %s without HTTP success/refresh", status, e, kind)
				}
			})
		}
	}
}
