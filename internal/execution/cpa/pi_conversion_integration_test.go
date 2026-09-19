//go:build pi_integration

package cpa

// Only the TLS upstream is synthetic. These tests exercise encrypted credential
// preparation, the real adapter, Node and the installed lockfile-pinned pi-ai.
import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"gpt-load/internal/execution"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/protocol"
	"gpt-load/internal/subscription"
)

const piConversionText = "The weather is sunny."
const piConversionCall = "call_weather_fixture"

func piConversionWire(t *testing.T) []byte {
	t.Helper()
	var wire bytes.Buffer
	sequence := 0
	emit := func(kind string, fields map[string]any) {
		fields["type"], fields["sequence_number"] = kind, sequence
		sequence++
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&wire, "event: %s\ndata: %s\n\n", kind, raw)
	}
	part := map[string]any{"type": "output_text", "text": piConversionText, "annotations": []any{}}
	message := map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "status": "completed", "content": []any{part}}
	tool := map[string]any{"id": "fc_fixture", "type": "function_call", "call_id": piConversionCall, "name": "weather", "arguments": `{"city":"Paris"}`, "status": "completed"}
	emit("response.created", map[string]any{"response": map[string]any{"id": "resp_conversion", "object": "response", "model": "gpt-5", "created_at": 1700000000, "status": "in_progress", "output": []any{}}})
	emit("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}})
	emit("response.content_part.added", map[string]any{"output_index": 0, "content_index": 0, "item_id": "msg_fixture", "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
	emit("response.output_text.delta", map[string]any{"output_index": 0, "content_index": 0, "item_id": "msg_fixture", "delta": piConversionText})
	emit("response.output_text.done", map[string]any{"output_index": 0, "content_index": 0, "item_id": "msg_fixture", "text": piConversionText})
	emit("response.content_part.done", map[string]any{"output_index": 0, "content_index": 0, "item_id": "msg_fixture", "part": part})
	emit("response.output_item.done", map[string]any{"output_index": 0, "item": message})
	emit("response.output_item.added", map[string]any{"output_index": 1, "item": map[string]any{"id": "fc_fixture", "type": "function_call", "call_id": piConversionCall, "name": "weather", "arguments": "", "status": "in_progress"}})
	emit("response.function_call_arguments.delta", map[string]any{"output_index": 1, "item_id": "fc_fixture", "delta": `{"city":`})
	emit("response.function_call_arguments.delta", map[string]any{"output_index": 1, "item_id": "fc_fixture", "delta": `"Paris"}`})
	emit("response.function_call_arguments.done", map[string]any{"output_index": 1, "item_id": "fc_fixture", "arguments": `{"city":"Paris"}`})
	emit("response.output_item.done", map[string]any{"output_index": 1, "item": tool})
	emit("response.completed", map[string]any{"response": map[string]any{"id": "resp_conversion", "object": "response", "model": "gpt-5", "created_at": 1700000000, "status": "completed", "output": []any{message, tool}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 4, "total_tokens": 14, "input_tokens_details": map[string]any{"cached_tokens": 7}}, "native_only_extension": "must-not-leak"}})
	return wire.Bytes()
}

func TestPiIntegrationConvertedProtocols(t *testing.T) {
	token := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"account-1"},"exp":4102444800}`)) + ".synthetic"
	fixture, db, _, keys, row := newAdapterFixture(t, credentialJSON(token, "synthetic-refresh", time.Now().Add(time.Hour)))
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if strings.Contains(row.Data, token) {
		t.Fatal("credential must be encrypted")
	}
	var calls atomic.Int32
	requests := make(chan []byte, 16)
	wire := piConversionWire(t)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/backend-api/codex/responses" {
			t.Errorf("upstream route: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Originator") != "pi" {
			t.Errorf("non-Pi transport originator: %q", r.Header.Get("Originator"))
		}
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Chatgpt-Account-Id") != "account-1" {
			t.Error("credential identity lost")
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if r.Header.Get("Content-Encoding") == "zstd" {
			decoder, err := zstd.NewReader(nil)
			if err != nil {
				t.Error(err)
				return
			}
			raw, err = decoder.DecodeAll(raw, nil)
			decoder.Close()
			if err != nil {
				t.Error(err)
				return
			}
		}
		select {
		case requests <- raw:
		default:
			t.Error("unexpected upstream retries")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(wire)
	}))
	t.Cleanup(upstream.Close)
	endpoint := startAdapterPi(t, upstream)
	adapter, err := NewAdapterWithConfig(fixture.credentials.(*subscription.CredentialManager), fixture.channels, &config.Config{ExperimentalPiEnabled: true, PiBridgeURL: endpoint, PiBridgeSecret: piIntegrationSecret})
	if err != nil {
		t.Fatal(err)
	}
	sentinel := &fakeExecutor{err: errors.New("CPA execution forbidden in Pi conversion tests")}
	setCodexExecutor(t, adapter, sentinel)
	t.Cleanup(func() {
		sentinel.mu.Lock()
		defer sentinel.mu.Unlock()
		if sentinel.calls != 0 || sentinel.countCalls != 0 {
			t.Errorf("CPA fallback calls=%d countCalls=%d", sentinel.calls, sentinel.countCalls)
		}
	})
	base := validSpec(t, row, keys)
	base.TargetConfig, err = json.Marshal(map[string]string{"execution_driver": "pi-experimental", "base_url": upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	// Prove the synthetic event sequence is accepted by the real Pi transport
	// even while converted-route admission is still RED during development.
	t.Run("native_fixture_control", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		before := calls.Load()
		got := adapter.Execute(ctx, base)
		if got.Error != nil {
			t.Fatalf("native fixture control: %+v", got.Error)
		}
		if got.Header.Get("X-GPT-Load-Driver") != "pi-experimental" || gjson.GetBytes(got.Body, "output.1.call_id").String() != piConversionCall || gjson.GetBytes(got.Body, "output.0.content.0.text").String() != piConversionText || got.Usage == nil || got.Usage.Normalized.Tokens.Output != 4 {
			t.Errorf("native fixture control lost content or usage: %+v body=%s", got.Usage, got.Body)
		}
		if calls.Load()-before != 1 {
			t.Errorf("native control attempts=%d", calls.Load()-before)
		}
		select {
		case <-requests:
		default:
			t.Error("native control did not reach TLS upstream")
		}
	})
	cases := []struct {
		name       string
		protocol   protocol.Protocol
		path, body string
	}{
		{"openai_chat", protocol.OpenAICompletions, "/v1/chat/completions", `{"model":"gpt-5","messages":[{"role":"system","content":"Keep instructions stable."},{"role":"user","content":"Weather in Paris?"}],"tools":[{"type":"function","function":{"name":"weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],"tool_choice":"auto"}`},
		{"anthropic", protocol.Anthropic, "/v1/messages", `{"model":"gpt-5","max_tokens":64,"system":"Keep instructions stable.","messages":[{"role":"user","content":"Weather in Paris?"}],"tools":[{"name":"weather","description":"Get weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],"tool_choice":{"type":"auto"}}`},
		{"gemini", protocol.Gemini, "/v1beta/models/gpt-5:generateContent", `{"systemInstruction":{"parts":[{"text":"Keep instructions stable."}]},"contents":[{"role":"user","parts":[{"text":"Weather in Paris?"}]}],"tools":[{"functionDeclarations":[{"name":"weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]}],"toolConfig":{"functionCallingConfig":{"mode":"AUTO"}}}`},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				spec := base
				spec.RouteMode = execution.RouteConverted
				spec.ClientProtocol = tc.protocol
				spec.Operation = execution.OperationChatCompletion
				spec.Path = tc.path
				spec.Body = []byte(tc.body)
				if stream && tc.protocol == protocol.Gemini {
					spec.Path = "/v1beta/models/gpt-5:streamGenerateContent"
					spec.RawQuery = "alt=sse"
				}
				if tc.protocol != protocol.Gemini {
					spec.Body, err = sjson.SetBytes(spec.Body, "stream", stream)
					if err != nil {
						t.Fatal(err)
					}
				}
				if stream && tc.protocol == protocol.OpenAICompletions {
					spec.Body, err = sjson.SetBytes(spec.Body, "stream_options.include_usage", true)
					if err != nil {
						t.Fatal(err)
					}
				}
				original := bytes.Clone(spec.Body)
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				before := calls.Load()
				var body []byte
				if stream {
					var data bytes.Buffer
					ready := 0
					got := adapter.ExecuteStream(ctx, spec, func(event execution.StreamEvent) error {
						if event.Kind == execution.StreamEventReady {
							ready++
							if event.Header.Get("X-GPT-Load-Driver") != "pi-experimental" || !strings.Contains(event.Header.Get("Content-Type"), "text/event-stream") {
								t.Errorf("stream metadata: %v", event.Header)
							}
						}
						if event.Kind == execution.StreamEventData {
							data.Write(event.Data)
						}
						return nil
					})
					if got.Error != nil {
						t.Fatalf("converted Pi stream: %+v", got.Error)
					}
					if err := got.Validate(); err != nil {
						t.Error(err)
					}
					if ready != 1 {
						t.Errorf("ready events=%d", ready)
					}
					body = data.Bytes()
				} else {
					got := adapter.Execute(ctx, spec)
					if got.Error != nil {
						t.Fatalf("converted Pi unary: %+v", got.Error)
					}
					if err := got.Validate(); err != nil {
						t.Error(err)
					}
					if got.Header.Get("X-GPT-Load-Driver") != "pi-experimental" || got.Header.Get("Content-Type") != "application/json" {
						t.Errorf("unary metadata: %v", got.Header)
					}
					body = got.Body
					// Keep adapter usage extraction identical to the existing client codec
					// baseline (Gemini's codec does not expose Responses cached_tokens).
					want := responseUsage(spec, body)
					if want == nil || got.Usage == nil || !reflect.DeepEqual(got.Usage.Normalized, want.Normalized) {
						t.Errorf("usage does not match client baseline: got=%+v want=%+v", got.Usage, want)
					}
				}
				if !bytes.Equal(spec.Body, original) {
					t.Error("caller body mutated")
				}
				if calls.Load()-before != 1 {
					t.Errorf("upstream attempts=%d", calls.Load()-before)
				}
				select {
				case raw := <-requests:
					piConversionAssertRequest(t, raw)
				default:
					t.Error("no Pi upstream request")
				}
				piConversionAssertClient(t, tc.protocol, stream, body)
			})
		}
	}
	// Reject only cases explicitly rejected by conversion_fidelity.go. Anthropic
	// has no analogous allowlist rejection there; do not invent stricter policy.
	for _, tc := range []struct {
		name  string
		index int
		path  string
		value any
	}{
		{"chat_unknown_allowed_tool", 0, "tool_choice", map[string]any{"type": "allowed_tools", "allowed_tools": map[string]any{"mode": "auto", "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "missing"}}}}}},
		{"gemini_unknown_allowed_tool", 2, "toolConfig.functionCallingConfig", map[string]any{"mode": "ANY", "allowedFunctionNames": []string{"missing"}}},
		{"gemini_invalid_allowlist_mode", 2, "toolConfig.functionCallingConfig", map[string]any{"mode": "NONE", "allowedFunctionNames": []string{"weather"}}},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("reject/%s/stream=%t", tc.name, stream), func(t *testing.T) {
				source := cases[tc.index]
				spec := base
				spec.RouteMode = execution.RouteConverted
				spec.Operation = execution.OperationChatCompletion
				spec.ClientProtocol = source.protocol
				spec.Path = source.path
				spec.Body, err = sjson.SetBytes([]byte(source.body), tc.path, tc.value)
				if err != nil {
					t.Fatal(err)
				}
				if stream && source.protocol == protocol.Gemini {
					spec.Path = "/v1beta/models/gpt-5:streamGenerateContent"
					spec.RawQuery = "alt=sse"
				}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				before := calls.Load()
				var evidence *execution.ErrorEvidence
				var dispatch execution.DispatchState
				if stream {
					got := adapter.ExecuteStream(ctx, spec, func(execution.StreamEvent) error { t.Error("rejected conversion emitted event"); return nil })
					evidence, dispatch = got.Error, got.DispatchState
				} else {
					got := adapter.Execute(ctx, spec)
					evidence, dispatch = got.Error, got.DispatchState
				}
				if evidence == nil || evidence.Kind != execution.ErrorKindConversionUnsupported || evidence.Code != execution.ErrorCodeCriticalSemanticLoss || dispatch != execution.DispatchNotSent {
					t.Errorf("semantic loss must be blocked: %+v dispatch=%s", evidence, dispatch)
				}
				if calls.Load() != before {
					t.Error("rejected conversion reached upstream")
				}
			})
		}
	}
}

func piConversionAssertRequest(t *testing.T, raw []byte) {
	t.Helper()
	root := gjson.ParseBytes(raw)
	if !gjson.ValidBytes(raw) || root.Get("model").String() != "gpt-5" || root.Get("store").Type != gjson.False || !root.Get("stream").Bool() || !root.Get("input").IsArray() {
		t.Errorf("native request envelope: %s", raw)
	}
	if root.Get("messages").Exists() || root.Get("contents").Exists() {
		t.Errorf("client wire leaked upstream: %s", raw)
	}
	foundSystem, foundUser := strings.Contains(root.Get("instructions").String(), "Keep instructions stable."), false
	for _, item := range root.Get("input").Array() {
		role := item.Get("role").String()
		text := item.Get("content").String()
		if role == "system" || role == "developer" {
			foundSystem = foundSystem || strings.Contains(text, "Keep instructions stable.")
		}
		if role == "user" {
			foundUser = foundUser || strings.Contains(text, "Weather in Paris?")
		}
	}
	if !foundSystem || !foundUser {
		t.Errorf("instructions/user semantics lost: %s", raw)
	}
	tools := root.Get("tools").Array()
	if len(tools) != 1 || tools[0].Get("type").String() != "function" || tools[0].Get("name").String() != "weather" || !strings.EqualFold(tools[0].Get("parameters.properties.city.type").String(), "string") || tools[0].Get("parameters.required.0").String() != "city" {
		t.Errorf("tool schema lost: %s", raw)
	}
}

func piConversionAssertClient(t *testing.T, p protocol.Protocol, stream bool, body []byte) {
	t.Helper()
	for _, leak := range []string{`"response.completed"`, `"response.output_text.delta"`, `"object":"response"`, `native_only_extension`} {
		if bytes.Contains(body, []byte(leak)) {
			t.Errorf("native wire leaked: %s", body)
		}
	}
	var frames []gjson.Result
	if stream {
		for _, line := range strings.Split(string(body), "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				continue
			}
			if !gjson.Valid(data) {
				t.Errorf("invalid SSE JSON: %s", data)
				continue
			}
			frames = append(frames, gjson.Parse(data))
		}
	} else {
		if !gjson.ValidBytes(body) {
			t.Fatalf("invalid client JSON: %s", body)
		}
		frames = []gjson.Result{gjson.ParseBytes(body)}
	}
	var text, args, id, name string
	var usage gjson.Result
	terminal := false
	for _, frame := range frames {
		switch p {
		case protocol.OpenAICompletions:
			object := "chat.completion"
			key := "message"
			if stream {
				object = "chat.completion.chunk"
				key = "delta"
			}
			if frame.Get("object").String() != object {
				t.Errorf("wrong Chat envelope: %s", frame.Raw)
			}
			message := frame.Get("choices.0." + key)
			text += message.Get("content").String()
			for _, tool := range message.Get("tool_calls").Array() {
				if tool.Get("id").String() != "" {
					id = tool.Get("id").String()
				}
				name += tool.Get("function.name").String()
				args += tool.Get("function.arguments").String()
			}
			if frame.Get("choices.0.finish_reason").String() == "tool_calls" {
				terminal = true
			}
			if frame.Get("usage.prompt_tokens").Exists() {
				usage = frame.Get("usage")
			}
		case protocol.Anthropic:
			if !stream {
				if frame.Get("type").String() != "message" || frame.Get("role").String() != "assistant" {
					t.Errorf("wrong Anthropic envelope: %s", frame.Raw)
				}
				for _, block := range frame.Get("content").Array() {
					if block.Get("type").String() == "text" {
						text += block.Get("text").String()
					}
					if block.Get("type").String() == "tool_use" {
						id = block.Get("id").String()
						name = block.Get("name").String()
						args = block.Get("input").Raw
					}
				}
				terminal = frame.Get("stop_reason").String() == "tool_use"
				usage = frame.Get("usage")
			} else {
				switch frame.Get("type").String() {
				case "content_block_start":
					block := frame.Get("content_block")
					if block.Get("type").String() == "tool_use" {
						id = block.Get("id").String()
						name = block.Get("name").String()
					}
				case "content_block_delta":
					text += frame.Get("delta.text").String()
					args += frame.Get("delta.partial_json").String()
				case "message_delta":
					terminal = frame.Get("delta.stop_reason").String() == "tool_use"
					usage = frame.Get("usage")
				}
			}
		case protocol.Gemini:
			if !frame.Get("candidates").IsArray() {
				t.Errorf("wrong Gemini envelope: %s", frame.Raw)
			}
			for _, part := range frame.Get("candidates.0.content.parts").Array() {
				text += part.Get("text").String()
				if call := part.Get("functionCall"); call.Exists() {
					id = call.Get("id").String()
					name = call.Get("name").String()
					args = call.Get("args").Raw
				}
			}
			if frame.Get("candidates.0.finishReason").String() == "STOP" {
				terminal = true
			}
			if frame.Get("usageMetadata.promptTokenCount").Exists() {
				usage = frame.Get("usageMetadata")
			}
		}
	}
	if text != piConversionText || id != piConversionCall || name != "weather" || !gjson.Valid(args) || gjson.Get(args, "city").String() != "Paris" || !terminal {
		t.Errorf("client semantics text=%q id=%q name=%q args=%q terminal=%t; wire=%s", text, id, name, args, terminal, body)
	}
	switch p {
	case protocol.OpenAICompletions:
		if usage.Get("prompt_tokens").Int() != 10 || usage.Get("completion_tokens").Int() != 4 || usage.Get("total_tokens").Int() != 14 || usage.Get("prompt_tokens_details.cached_tokens").Int() != 7 {
			t.Errorf("Chat usage: %s", usage.Raw)
		}
	case protocol.Anthropic:
		if usage.Get("input_tokens").Int() != 3 || usage.Get("output_tokens").Int() != 4 || usage.Get("cache_read_input_tokens").Int() != 7 {
			t.Errorf("Anthropic usage: %s", usage.Raw)
		}
	case protocol.Gemini:
		// Shared CPA codec baseline reports total input, not a cached breakdown.
		if usage.Get("promptTokenCount").Int() != 10 || usage.Get("candidatesTokenCount").Int() != 4 || usage.Get("totalTokenCount").Int() != 14 {
			t.Errorf("Gemini usage: %s", usage.Raw)
		}
	}
}
