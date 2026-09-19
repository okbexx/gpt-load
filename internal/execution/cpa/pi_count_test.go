package cpa

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"gpt-load/internal/subscription/providers/codex"
)

// Every executor method panics: production local count must not reach CPA or Pi.
type piCountForbiddenExecutor struct{}

func (piCountForbiddenExecutor) Execute(context.Context, string, codex.Credential, codex.ExecuteRequest) (codex.ExecuteResponse, error) {
	panic("unexpected execution")
}
func (piCountForbiddenExecutor) CountTokens(context.Context, string, codex.Credential, codex.ExecuteRequest) (codex.ExecuteResponse, error) {
	panic("unexpected CPA count")
}
func (piCountForbiddenExecutor) ExecuteStream(context.Context, string, codex.Credential, codex.ExecuteRequest) (*codex.ExecuteStreamResponse, error) {
	panic("unexpected stream")
}

func TestPiCountLocalWithoutExecutorOrCredentials(t *testing.T) {
	for _, bridge := range []*piProviderBridge{{}, {base: &codexProviderBridge{executor: piCountForbiddenExecutor{}}}} {
		q := providerRequest{Model: "gpt-5", Format: "openai-response", Payload: []byte(`{"input":"hello"}`), BaseURL: "not a URL", ProxyURL: "not a proxy"}
		if err := bridge.ValidateLocalTokenCount(q); err != nil {
			t.Fatal(err)
		}
		r, err := bridge.CountTokensLocal(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		if r.Headers.Get(localTokenCountHeader) != "local-estimate" || r.Headers.Get("X-GPT-Load-Driver") != "pi-experimental" {
			t.Fatalf("headers: %v", r.Headers)
		}
		var body struct {
			Object string `json:"object"`
			Count  int    `json:"input_tokens"`
		}
		if json.Unmarshal(r.Payload, &body) != nil || body.Object != "response.input_tokens" || body.Count <= 0 {
			t.Fatalf("body: %s", r.Payload)
		}
		if !r.Local || r.UpstreamProtocol != "" || !r.QuotaObservedAt.IsZero() || len(r.QuotaWindows) != 0 {
			t.Fatalf("not local: %+v", r)
		}
		q.Payload = []byte(`{"input":"hello","tools":[]}`)
		if _, err := bridge.CountTokensLocal(context.Background(), q); err == nil {
			t.Fatal("unsupported direct count accepted")
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		q.Payload = []byte(`{"input":"hello"}`)
		if _, err := bridge.CountTokensLocal(ctx, q); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
	}
}

func TestPiCountValidationMatchesBaseline(t *testing.T) {
	pi := &piProviderBridge{}
	base := &codexProviderBridge{}
	for _, format := range []string{"openai-response", "claude", "gemini", "openai"} {
		for _, model := range []string{"gpt-5", "gpt-4.1", "gpt-4o", "gpt-4", "gpt-3", "other", ""} {
			for _, body := range []string{`{}`, `null`, `{"input":null}`, `{"input":"hello"}`, `{"instructions":null}`, `{"input":[{"type":"message","role":"user","content":[]}]}`, `{"input":[{"type":{},"content":"hi"}]}`, `{"input":[{"content":[{"type":"input_image","text":"hi"}]}]}`, `{"messages":[]}`, `{"messages":[{"role":null,"content":"hi"}]}`, `{"system":[{"type":"text","text":"hi"}],"messages":[]}`, `{"contents":[]}`, `{"contents":[{"role":42,"parts":[{"text":"hi"}]}]}`, `{"tools":[]}`, `{"unknown":true}`} {
				q := providerRequest{Model: model, Format: format, Payload: []byte(body)}
				a, b := pi.ValidateLocalTokenCount(q), base.ValidateLocalTokenCount(q)
				if (a == nil) != (b == nil) {
					t.Fatalf("%s/%s/%s: Pi=%v baseline=%v", model, format, body, a, b)
				}
			}
		}
	}
}
