package cpa

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"gpt-load/internal/execution"
	"gpt-load/internal/health"
)

// Use the HTTP bridge/frame reader, not constructed errors: admission statuses
// belong to the sidecar, never to the selected upstream account.
func TestPiAdmissionErrors(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			code   string
			status int
			want   string
			kind   execution.ErrorKind
		}{
			{"busy", 503, "pi_busy", execution.ErrorKindInternal},
			{"overloaded", 429, "pi_overloaded", execution.ErrorKindInternal},
			{"unauthorized", 401, "pi_unauthorized", execution.ErrorKindInternal},
			{"internal_error", 500, "pi_internal_error", execution.ErrorKindInternal},
			{"PRIVATE\ncredential", 503, "pi_bridge_error", execution.ErrorKindInternal},
			{"unsupported_capability", 400, "pi_unsupported_capability", execution.ErrorKindConversionUnsupported},
			{"invalid_request", 400, "pi_invalid_request", execution.ErrorKindInvalidRequest},
		} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.want, stream), func(t *testing.T) {
				var calls atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header().Set("Content-Type", "application/x-ndjson")
					w.WriteHeader(tc.status)
					_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "code": tc.code, "status": tc.status, "dispatch_state": "not_sent", "retry_after_seconds": 17})
				}))
				defer srv.Close()
				a, spec := piErrorFixture(t, srv.URL)
				fallback := &fakeExecutor{}
				setCodexExecutor(t, a, fallback)
				var e *execution.ErrorEvidence
				if stream {
					got := a.ExecuteStream(t.Context(), spec, func(execution.StreamEvent) error { t.Error("admission emitted event"); return nil })
					if err := got.Validate(); err != nil {
						t.Errorf("invalid stream result: %v; %+v", err, got)
					}
					if got.DispatchState != execution.DispatchNotSent || got.ResponseStarted || got.StatusCode != 0 || len(got.Header) != 0 {
						t.Errorf("unsafe stream metadata: %+v", got)
					}
					e = got.Error
				} else {
					got := a.Execute(t.Context(), spec)
					if err := got.Validate(); err != nil {
						t.Errorf("invalid attempt result: %v; %+v", err, got)
					}
					if got.DispatchState != execution.DispatchNotSent || got.ResponseStarted || got.StatusCode != 0 || len(got.Header) != 0 || len(got.Body) != 0 {
						t.Errorf("unsafe unary metadata: %+v", got)
					}
					e = got.Error
				}
				if e == nil {
					t.Fatal("missing error")
				}
				if e.Kind != tc.kind || e.Code != tc.want || e.Type != "pi_driver_error" || e.StatusCode != 0 || e.Summary != "experimental pi driver: "+tc.want || e.Hint != "" || e.ReplaySafety != execution.ReplaySafetyUnknown {
					t.Errorf("admission taxonomy: %+v", e)
				}
				if tc.kind == execution.ErrorKindInternal {
					assertPiLocalAdmission(t, e, tc.want)
				}
				if calls.Load() != 1 || fallback.calls != 0 {
					t.Errorf("retry/fallback: sidecar=%d CPA=%d", calls.Load(), fallback.calls)
				}
			})
		}
	}
}

func assertPiLocalAdmission(t *testing.T, e *execution.ErrorEvidence, code string) {
	t.Helper()
	if e == nil {
		t.Fatal("missing local admission error")
	}
	if e.Kind != execution.ErrorKindInternal || e.OriginHint != execution.ErrorOriginInternal || e.Code != code || e.StatusCode != 0 || e.Hint != "" || e.ScopeHint != "" || e.RetryAfter != 0 || e.ReplaySafety != execution.ReplaySafetyUnknown {
		t.Errorf("local admission evidence: %+v", e)
	}
	decision := health.JudgeExecution(health.ExecutionAttempt{DispatchState: execution.DispatchNotSent, Evidence: e}, health.DecisionContext{})
	if decision.Retry != health.RetryNone || decision.Effect != health.EffectNone || decision.Origin != execution.ErrorOriginInternal {
		t.Errorf("sidecar failure affected upstream health/retry: %+v", decision)
	}
}
