package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPiConfig(t *testing.T) {
	for _, endpoint := range []string{"", "http://example.com", "http://localhost:1234", "http://127.0.0.1:1234/path", "http://user@127.0.0.1:1234", "http://127.0.0.1:1234?x=y"} {
		if _, err := NewPiExecutor(endpoint, strings.Repeat("x", 32)); err == nil {
			t.Fatalf("accepted %q", endpoint)
		}
	}
	if _, err := NewPiExecutor("http://127.0.0.1:1234", "short"); err == nil {
		t.Fatal("accepted weak secret")
	}
}
func TestPiUnaryContract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/execute" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("x", 32) {
			t.Error("wrong transport")
		}
		var request map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("invalid request")
		}
		if string(request["provider"]) != `"codex"` || string(request["operation"]) != `"responses"` {
			t.Error("wrong envelope")
		}
		if strings.Contains(string(request["credential"]), "refresh") || strings.Contains(string(request["headers"]), "evil") {
			t.Error("credential/header isolation")
		}
		fmt.Fprintln(w, `{"type":"headers","driver":"pi","status":200,"headers":{"x-request-id":"pi-1"},"dispatch_state":"maybe_sent"}`)
		fmt.Fprintln(w, `{"type":"result","response":{"id":"resp_1","object":"response","output":[]}}`)
		fmt.Fprintln(w, `{"type":"done","dispatch_state":"maybe_sent"}`)
	}))
	defer srv.Close()
	e, err := NewPiExecutor(srv.URL, strings.Repeat("x", 32))
	if err != nil {
		t.Fatal(err)
	}
	response, err := e.Execute(t.Context(), "id", Credential{AccessToken: "access", RefreshToken: "refresh-secret", AccountID: "account"}, ExecuteRequest{Model: "gpt-5", Format: "openai-response", Payload: []byte(`{"input":"hello"}`), Headers: http.Header{"Authorization": []string{"evil"}}})
	if err != nil || response.Headers.Get("X-GPT-Load-Driver") != "pi-experimental" || response.Headers.Get("X-Request-Id") != "" {
		t.Fatalf("response=%+v err=%v", response, err)
	}
}
func TestPiFrameFailures(t *testing.T) {
	for _, frames := range []string{
		"", `{"type":"result","response":{}}`,
		"{\"type\":\"headers\",\"driver\":\"pi\",\"status\":200,\"dispatch_state\":\"maybe_sent\"}\n",
		"{\"type\":\"headers\",\"driver\":\"pi\",\"status\":200,\"dispatch_state\":\"maybe_sent\"}\n{\"type\":\"done\",\"dispatch_state\":\"maybe_sent\"}\n",
	} {
		t.Run(fmt.Sprint(len(frames)), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, frames) }))
			defer srv.Close()
			e, _ := NewPiExecutor(srv.URL, strings.Repeat("x", 32))
			_, err := e.Execute(t.Context(), "", Credential{}, ExecuteRequest{Format: "openai-response", Payload: []byte(`{}`)})
			var pe *PiError
			if !errors.As(err, &pe) || pe.DispatchState() != "maybe_sent" {
				t.Fatalf("unsafe error: %v", err)
			}
		})
	}
}
func TestPiUnsupportedAndTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("unsupported request dispatched") }))
	e, _ := NewPiExecutor(srv.URL, strings.Repeat("x", 32))
	for _, req := range []ExecuteRequest{{Format: "openai"}, {Format: "openai-response", RequestPath: "/v1/alpha/search"}} {
		_, err := e.Execute(context.Background(), "", Credential{}, req)
		var pe *PiError
		if !errors.As(err, &pe) || pe.DispatchState() != "not_sent" {
			t.Fatalf("error=%v", err)
		}
	}
	_, err := e.CountTokens(t.Context(), "", Credential{}, ExecuteRequest{})
	if err == nil {
		t.Fatal("count supported")
	}
	srv.Close()
	_, err = e.Execute(t.Context(), "", Credential{}, ExecuteRequest{Format: "openai-response", Payload: []byte(`{}`)})
	var pe *PiError
	if !errors.As(err, &pe) || pe.DispatchState() != "maybe_sent" {
		t.Fatalf("transport=%v", err)
	}
}

func TestPiSafeQuotaMetadata(t *testing.T) {
	safe := map[string]string{"x-codex-primary-used-percent": "12.5", "x-codex-primary-window-minutes": "300", "x-codex-primary-reset-after-seconds": "60", "x-codex-allowed": "true", "x-codex-active-limit": "premium"}
	input := map[string]string{}
	for k, v := range safe {
		input[k] = v
	}
	for k, v := range map[string]string{"x-codex-secret": "SECRET", "x-request-id": "SECRET", "set-cookie": "SECRET", "x-codex-limit-name": "SECRET", "x-codex-secondary-used-percent": "101", "x-codex-secondary-reset-at": "NaN"} {
		input[k] = v
	}
	input["retry-after"] = "7"
	h, q := piHeaders(piFrame{Headers: input})
	if len(q) != len(safe) {
		t.Fatalf("unsafe/lost quota: %#v", q)
	}
	for k, v := range safe {
		if q[k] != v {
			t.Errorf("%s = %q", k, q[k])
		}
	}
	if h.Get("Retry-After") != "7" || h.Get("X-Request-Id") != "" {
		t.Fatalf("headers %#v", h)
	}
	if len(NormalizePassiveQuotaWindows(q, time.Now())) != 1 {
		t.Fatal("quota not consumable")
	}
}
func TestPiRetryAfterFromBridge(t *testing.T) {
	for _, value := range []string{"17", "0", "-1", "1.5", "31622401", `"SECRET"`, "null"} {
		t.Run(value, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				fmt.Fprintf(w, `{"type":"error","code":"upstream_http_error","status":429,"dispatch_state":"maybe_sent","retry_after_seconds":%s}`+"\n", value)
			}))
			defer srv.Close()
			e, _ := NewPiExecutor(srv.URL, strings.Repeat("x", 32))
			_, err := e.Execute(t.Context(), "", Credential{}, ExecuteRequest{Format: "openai-response", Payload: []byte(`{}`)})
			var retry interface{ RetryAfter() *time.Duration }
			if !errors.As(err, &retry) {
				t.Fatalf("missing RetryAfter: %v", err)
			}
			got := retry.RetryAfter()
			if value == "17" || value == "0" {
				want := time.Duration(0)
				if value == "17" {
					want = 17 * time.Second
				}
				if got == nil || *got != want {
					t.Fatalf("retry=%v want=%v", got, want)
				}
			} else if got != nil {
				t.Fatalf("unsafe delay: %v", *got)
			}
			var pe *PiError
			if !errors.As(err, &pe) || pe.DispatchState() != "maybe_sent" || strings.Contains(err.Error(), "SECRET") || calls != 1 {
				t.Fatalf("unsafe error=%v calls=%d", err, calls)
			}
		})
	}
}
