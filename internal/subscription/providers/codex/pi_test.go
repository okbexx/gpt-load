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
	if err != nil || response.Headers.Get("X-GPT-Load-Driver") != "pi-experimental" || response.Headers.Get("X-Request-Id") != "pi-1" {
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
