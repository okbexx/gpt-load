package cpa

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpt-load/internal/execution"
	"gpt-load/internal/protocol"
	"gpt-load/internal/subscription/providers/codex"
)

func TestPiContinuityIsCredentialScoped(t *testing.T) {
	first := scopedPiContinuityKey("tenant-session", 11, 3, "gpt-5")
	second := scopedPiContinuityKey("tenant-session", 12, 3, "gpt-5")
	generation := scopedPiContinuityKey("tenant-session", 11, 4, "gpt-5")
	model := scopedPiContinuityKey("tenant-session", 11, 3, "gpt-5-mini")
	if first == "" || first == second || first == generation || first == model {
		t.Fatalf("Pi continuity key is not scoped: %q %q %q %q", first, second, generation, model)
	}
	if scopedPiContinuityKey("", 11, 3, "gpt-5") != "" {
		t.Fatal("empty tenant continuity key should remain disabled")
	}
}
func TestExperimentalPiSelectionIsolation(t *testing.T) {
	adapter, _, _, keys, row := newAdapterFixture(t, credentialJSON("access", "refresh", time.Now().Add(time.Hour)))
	cpa := &fakeExecutor{result: codex.ExecuteResponse{Payload: []byte(`{"id":"cpa","output":[]}`)}}
	setCodexExecutor(t, adapter, cpa)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprintln(w, `{"type":"headers","driver":"pi","status":200,"dispatch_state":"maybe_sent"}`)
		fmt.Fprintln(w, `{"type":"result","response":{"id":"pi","object":"response","status":"completed","output":[]}}`)
		fmt.Fprintln(w, `{"type":"done","dispatch_state":"maybe_sent"}`)
	}))
	defer srv.Close()
	pi, err := codex.NewPiExecutor(srv.URL, strings.Repeat("x", 32))
	if err != nil {
		t.Fatal(err)
	}
	adapter.piExecutor = pi
	spec := validSpec(t, row, keys)
	if got := adapter.Execute(t.Context(), spec); got.Error != nil {
		t.Fatalf("default: %+v", got)
	}
	if cpa.calls != 1 || calls != 0 {
		t.Fatal("default changed")
	}
	spec.ClientProtocol = protocol.OpenAIResponses
	spec.RouteMode = execution.RouteNative
	spec.Operation = execution.OperationResponsesCreate
	spec.Path = "/v1/responses"
	spec.Body = []byte(`{"model":"gpt-5","input":"hello"}`)
	spec.TargetConfig = []byte(`{"execution_driver":"pi-experimental"}`)
	got := adapter.Execute(t.Context(), spec)
	if got.Error != nil || got.Header.Get("X-GPT-Load-Driver") != "pi-experimental" || calls != 1 || cpa.calls != 1 {
		t.Fatalf("pi: %+v, calls %d/%d", got, calls, cpa.calls)
	}
	srv.Close()
	got = adapter.Execute(t.Context(), spec)
	if got.Error == nil || got.DispatchState != execution.DispatchMaybeSent || cpa.calls != 1 {
		t.Fatalf("fallback/unsafe replay: %+v", got)
	}
	adapter.piExecutor = nil
	got = adapter.Execute(t.Context(), spec)
	if got.Error == nil || got.DispatchState != execution.DispatchNotSent || cpa.calls != 1 {
		t.Fatalf("disabled: %+v", got)
	}
}
func TestExperimentalPiUnsupportedBeforeCredentialPreparation(t *testing.T) {
	adapter, _, _, keys, row := newAdapterFixture(t, credentialJSON("access", "refresh", time.Now().Add(time.Hour)))
	preparer := &fakeCredentialPreparer{}
	adapter.credentials = preparer
	spec := validSpec(t, row, keys)
	spec.TargetConfig = []byte(`{"execution_driver":"pi-experimental"}`)
	got := adapter.Execute(t.Context(), spec)
	if got.Error == nil || got.DispatchState != execution.DispatchNotSent || preparer.calls != 0 {
		t.Fatalf("unsupported: %+v", got)
	}
	spec.ClientProtocol = protocol.OpenAIResponses
	spec.RouteMode = execution.RouteNative
	spec.Operation = execution.OperationResponsesCreate
	spec.Path = "/v1/responses"
	spec.Body = []byte(`{"input":"hello"}`)
	_, ws := adapter.OpenWebsocket(t.Context(), spec)
	if ws.Error == nil || ws.DispatchState != execution.DispatchNotSent || preparer.calls != 0 {
		t.Fatalf("websocket: %+v", ws)
	}
}
