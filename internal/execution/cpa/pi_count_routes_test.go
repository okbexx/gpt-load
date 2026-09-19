package cpa

import (
	"encoding/json"
	"testing"
	"time"

	"gpt-load/internal/execution"
	"gpt-load/internal/protocol"
)

func TestPiCountRoutesStayLocalThroughAdapter(t *testing.T) {
	for _, tc := range []struct {
		name              string
		client            protocol.Protocol
		mode              execution.RouteMode
		op                execution.Operation
		path, body, field string
	}{
		{"responses", protocol.OpenAIResponses, execution.RouteNative, execution.OperationResponsesInputTokens, "/v1/responses/input_tokens", `{"input":"hello world"}`, "input_tokens"},
		{"anthropic", protocol.Anthropic, execution.RouteConverted, execution.OperationCountTokens, "/v1/messages/count_tokens", `{"messages":[{"role":"user","content":"hello world"}]}`, "input_tokens"},
		{"gemini", protocol.Gemini, execution.RouteConverted, execution.OperationCountTokens, "/v1beta/models/gpt-5:countTokens", `{"contents":[{"role":"user","parts":[{"text":"hello world"}]}]}`, "totalTokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, _, keys, row := newAdapterFixture(t, credentialJSON("unused", "unused-refresh", time.Now().Add(time.Hour)))
			a.piExecutor = piCountForbiddenExecutor{}
			setCodexExecutor(t, a, piCountForbiddenExecutor{})
			preparer := &fakeCredentialPreparer{}
			a.credentials = preparer
			spec := validSpec(t, row, keys)
			spec.TargetConfig = json.RawMessage(`{"execution_driver":"pi-experimental"}`)
			spec.ClientProtocol, spec.RouteMode, spec.Operation, spec.Path, spec.Body = tc.client, tc.mode, tc.op, tc.path, []byte(tc.body)
			got := a.Execute(t.Context(), spec)
			if got.Error != nil || got.DispatchState != execution.DispatchLocal || got.Validate() != nil || preparer.calls != 0 {
				t.Fatalf("local count route failed or prepared credentials: %+v; prepares=%d", got, preparer.calls)
			}
			if got.Header.Get(localTokenCountHeader) != "local-estimate" || got.Header.Get("X-GPT-Load-Driver") != "pi-experimental" || got.Usage != nil {
				t.Fatalf("wrong local diagnostics/accounting: %+v", got)
			}
			var body map[string]any
			if json.Unmarshal(got.Body, &body) != nil {
				t.Fatalf("invalid response %s", got.Body)
			}
			if count, ok := body[tc.field].(float64); !ok || count <= 0 {
				t.Fatalf("missing count: %s", got.Body)
			}
			spec.Body = []byte(`{"tools":[]}`)
			bad := a.Execute(t.Context(), spec)
			if bad.Error == nil || bad.DispatchState != execution.DispatchNotSent || preparer.calls != 0 {
				t.Fatalf("unsupported count not rejected locally: %+v", bad)
			}
		})
	}
}
