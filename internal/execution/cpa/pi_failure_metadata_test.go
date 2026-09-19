package cpa

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gpt-load/internal/execution"
	"gpt-load/internal/subscription/providers/codex"
)

func TestPiUnaryFailureMetadata(t *testing.T) {
	const headers = `{"type":"headers","driver":"pi","status":200,"dispatch_state":"maybe_sent","headers":{"retry-after":"17","x-request-id":"PRIVATE","set-cookie":"PRIVATE","x-gpt-load-driver":"PRIVATE"}}`
	for _, tc := range []struct {
		name, frames, code string
		dispatch           execution.DispatchState
		retry              time.Duration
	}{
		{"failed_terminal", headers + "\n" + `{"type":"result","response":{"status":"failed"}}` + "\n" + `{"type":"done","dispatch_state":"maybe_sent"}`, "pi_bridge_protocol_error", execution.DispatchMaybeSent, 0},
		{"missing_terminal", headers, "pi_bridge_protocol_error", execution.DispatchMaybeSent, 0},
		{"timeout_after_headers", headers + "\n" + `{"type":"error","code":"timeout","status":200,"dispatch_state":"maybe_sent","retry_after_seconds":3}`, "pi_timeout", execution.DispatchMaybeSent, 3 * time.Second},
		{"not_sent", `{"type":"error","code":"timeout","status":408,"dispatch_state":"not_sent","retry_after_seconds":3}`, "pi_timeout", execution.DispatchNotSent, 3 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, tc.frames) }))
			defer srv.Close()
			a, spec := piErrorFixture(t, srv.URL)
			fallback := &fakeExecutor{}
			setCodexExecutor(t, a, fallback)
			got := a.Execute(t.Context(), spec)
			if err := got.Validate(); err != nil {
				t.Fatalf("invalid result: %v; %+v", err, got)
			}
			if got.Error == nil {
				t.Fatal("failed terminal accepted as success")
			}
			if got.DispatchState != tc.dispatch || got.ResponseStarted || got.StatusCode != 0 || len(got.Header) != 0 || len(got.Body) != 0 || fallback.calls != 0 {
				t.Fatalf("unsafe response/fallback: %+v", got)
			}
			e := got.Error
			if e.Type != "pi_driver_error" || e.Code != tc.code || e.StatusCode != 0 || e.ReplaySafety != execution.ReplaySafetyUnknown || e.RetryAfter != tc.retry {
				t.Fatalf("lost identity/retry: %+v", e)
			}
			want := "17"
			if tc.dispatch == execution.DispatchNotSent {
				want = ""
			}
			if e.Header.Get("Retry-After") != want || (want != "" && len(e.Header) != 1) || (want == "" && len(e.Header) != 0) {
				t.Fatalf("lost/unsafe evidence metadata: %#v", e.Header)
			}
		})
	}
}

func TestPiUnaryHTTPFailureMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"type":"headers","driver":"pi","status":200,"dispatch_state":"maybe_sent","headers":{"retry-after":"17","set-cookie":"PRIVATE"}}`)
		fmt.Fprintln(w, `{"type":"error","code":"upstream_http_error","status":429,"dispatch_state":"maybe_sent","retry_after_seconds":3}`)
	}))
	defer srv.Close()
	a, spec := piErrorFixture(t, srv.URL)
	got := a.Execute(t.Context(), spec)
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
	if got.Error == nil || got.DispatchState != execution.DispatchMaybeSent || !got.ResponseStarted || got.StatusCode != 429 || len(got.Body) != 0 {
		t.Fatalf("invalid HTTP failure: %+v", got)
	}
	if got.Header.Get("X-GPT-Load-Driver") != "pi-experimental" || got.Header.Get("Retry-After") != "17" || got.Header.Get("Set-Cookie") != "" || got.Error.Header.Get("Retry-After") != "17" || len(got.Error.Header) != 1 || got.Error.Type != "pi_driver_error" || got.Error.Code != "pi_upstream_http_error" || got.Error.RetryAfter != 3*time.Second || got.Error.ReplaySafety != execution.ReplaySafetyUnknown {
		t.Fatalf("lost HTTP failure metadata: %+v / %+v", got, got.Error)
	}
}

func TestDefaultCPAUnaryFailureMetadataUnchanged(t *testing.T) {
	a, _, _, keys, row := newAdapterFixture(t, credentialJSON("access", "refresh", time.Now().Add(time.Hour)))
	f := &fakeExecutor{result: codex.ExecuteResponse{StatusCode: 200, Headers: http.Header{"Retry-After": []string{"17"}, "X-GPT-Load-Driver": []string{"pi-experimental"}}}, err: errors.New("read failed")}
	setCodexExecutor(t, a, f)
	got := a.Execute(t.Context(), validSpec(t, row, keys))
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
	if got.Error == nil || got.ResponseStarted || got.StatusCode != 0 || len(got.Header) != 0 || len(got.Error.Header) != 0 || got.Error.Type == "pi_driver_error" || f.calls != 1 {
		t.Fatalf("default CPA changed: %+v", got)
	}
}
