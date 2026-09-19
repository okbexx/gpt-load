package codex

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type piCodecCapture struct {
	request   ExecuteRequest
	calls     int
	response  []byte
	frames    [][]byte
	err       error
	chunkErr  error
	cancelled chan struct{}
}

func (c *piCodecCapture) Execute(_ context.Context, _ string, _ Credential, q ExecuteRequest) (ExecuteResponse, error) {
	c.calls++
	c.request = q
	return ExecuteResponse{StatusCode: 200, Payload: c.response, Headers: http.Header{"X-Request-Id": []string{"trace"}}, UpstreamRequestPath: "/v1/responses", QuotaSignals: map[string]string{"remaining": "42"}}, c.err
}
func (c *piCodecCapture) CountTokens(context.Context, string, Credential, ExecuteRequest) (ExecuteResponse, error) {
	panic("count must not dispatch")
}
func (c *piCodecCapture) ExecuteStream(ctx context.Context, _ string, _ Credential, q ExecuteRequest) (*ExecuteStreamResponse, error) {
	c.calls++
	c.request = q
	if c.err != nil {
		return nil, c.err
	}
	out := make(chan ExecuteStreamChunk)
	go func() {
		defer close(out)
		if c.cancelled != nil {
			defer close(c.cancelled)
		}
		for _, b := range c.frames {
			select {
			case out <- ExecuteStreamChunk{Payload: b}:
			case <-ctx.Done():
				return
			}
		}
		if c.chunkErr != nil {
			select {
			case out <- ExecuteStreamChunk{Err: c.chunkErr}:
			case <-ctx.Done():
				return
			}
		}
		if c.cancelled != nil {
			<-ctx.Done()
		}
	}()
	return &ExecuteStreamResponse{Chunks: out, Headers: http.Header{"X-Request-Id": []string{"trace"}}, UpstreamRequestPath: "/v1/responses"}, nil
}

var piCodecCases = []struct{ format, path, body, toolPath, idPath, argsPath, finishPath, finish, usagePath, reasonPath string }{
	{"openai", "/v1/chat/completions", `{"model":"gpt-5","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`, "choices.0.message.tool_calls", "id", "function.arguments", "choices.0.finish_reason", "tool_calls", "usage.prompt_tokens", "choices.0.message.reasoning_content"},
	{"claude", "/v1/messages", `{"model":"gpt-5","messages":[{"role":"user","content":"hello"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`, "content.#(type==\"tool_use\")#", "id", "input", "stop_reason", "tool_use", "usage.input_tokens", "content.0.thinking"},
	{"gemini", "/v1beta/models/gpt-5:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"object"}}]}]}`, "candidates.0.content.parts.#(functionCall)#", "functionCall.id", "functionCall.args", "candidates.0.finishReason", "STOP", "usageMetadata.promptTokenCount", "candidates.0.content.parts.0.text"},
}

const piCodecResult = `{"id":"resp_fixture","created_at":100,"model":"gpt-5","status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"consider"}]},{"type":"function_call","id":"fc_1","call_id":"call_a","name":"lookup","arguments":"{\"x\":1}"},{"type":"function_call","id":"fc_2","call_id":"call_b","name":"lookup","arguments":"{\"x\":2}"}],"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"output_tokens_details":{"reasoning_tokens":2}}}`

func TestPiConvertedUnary(t *testing.T) {
	for _, tc := range piCodecCases {
		t.Run(tc.format, func(t *testing.T) {
			cap := &piCodecCapture{response: []byte(piCodecResult)}
			q := ExecuteRequest{Format: tc.format, RequestPath: tc.path, Model: "gpt-5", Payload: []byte(tc.body), ContinuityKey: "session", BaseURL: "http://fixture", ProxyURL: "direct"}
			got, err := NewPiConvertedExecutor(cap).Execute(context.Background(), "credential", Credential{}, q)
			if err != nil {
				t.Fatal(err)
			}
			if cap.calls != 1 || cap.request.Format != "openai-response" || cap.request.RequestPath != "/v1/responses" || cap.request.ContinuityKey != "session" || cap.request.BaseURL != q.BaseURL || cap.request.ProxyURL != q.ProxyURL {
				t.Fatalf("native dispatch: %+v", cap.request)
			}
			if !gjson.GetBytes(cap.request.Payload, "input").IsArray() || gjson.GetBytes(cap.request.Payload, "stream").Bool() {
				t.Fatalf("native body %s", cap.request.Payload)
			}
			if got.Headers.Get("X-Request-Id") != "trace" || got.QuotaSignals["remaining"] != "42" {
				t.Fatal("lost metadata")
			}
			tools := gjson.GetBytes(got.Payload, tc.toolPath).Array()
			if len(tools) != 2 {
				t.Fatalf("lost parallel tools: %s", got.Payload)
			}
			for i, id := range []string{"call_a", "call_b"} {
				if tools[i].Get(tc.idPath).String() != id {
					t.Fatalf("tool ID %s", got.Payload)
				}
				args := tools[i].Get(tc.argsPath)
				if args.Type == gjson.String {
					args = gjson.Parse(args.String())
				}
				if args.Get("x").Int() != int64(i+1) {
					t.Fatalf("arguments %s", got.Payload)
				}
			}
			if gjson.GetBytes(got.Payload, tc.finishPath).String() != tc.finish || gjson.GetBytes(got.Payload, tc.usagePath).Int() != 10 || (tc.format != "gemini" && gjson.GetBytes(got.Payload, tc.reasonPath).String() != "consider") {
				t.Fatalf("finish/usage/reasoning %s", got.Payload)
			}
		})
	}
}
func piCodecEvent(s string) []byte {
	return []byte("event: " + gjson.Get(s, "type").String() + "\r\ndata: " + s + "\r\n\r\n")
}
func TestPiConvertedStream(t *testing.T) {
	for _, tc := range piCodecCases {
		t.Run(tc.format, func(t *testing.T) {
			frames := [][]byte{piCodecEvent(`{"type":"response.created","response":{"id":"resp_fixture","model":"gpt-5","created_at":100}}`), piCodecEvent(`{"type":"response.output_text.delta","delta":"hello"}`), piCodecEvent(`{"type":"response.reasoning_summary_text.delta","delta":"consider"}`), piCodecEvent(`{"type":"response.completed","response":` + piCodecResult + `}`)}
			// Split every event across transport chunks, including CRLF boundaries.
			var fragments [][]byte
			for _, b := range frames {
				for _, v := range b {
					fragments = append(fragments, []byte{v})
				}
			}
			cap := &piCodecCapture{frames: fragments}
			q := ExecuteRequest{Format: tc.format, Model: "gpt-5", Payload: []byte(tc.body)}
			got, err := NewPiConvertedExecutor(cap).ExecuteStream(context.Background(), "id", Credential{}, q)
			if err != nil {
				t.Fatal(err)
			}
			var all strings.Builder
			for ch := range got.Chunks {
				if ch.Err != nil {
					t.Fatal(ch.Err)
				}
				all.Write(ch.Payload)
			}
			if !strings.Contains(all.String(), "hello") || !strings.Contains(all.String(), "consider") || !strings.Contains(all.String(), "10") {
				t.Fatalf("stream data %s", all.String())
			}
			if !gjson.GetBytes(cap.request.Payload, "stream").Bool() {
				t.Fatal("native stream mismatch")
			}
		})
	}
}
func TestPiConvertedOriginalToolNameAndNativePassthrough(t *testing.T) {
	long := strings.Repeat("lookup_", 12)
	for _, tc := range piCodecCases {
		t.Run(tc.format, func(t *testing.T) {
			body := strings.ReplaceAll(tc.body, "lookup", long)
			q := ExecuteRequest{Format: tc.format, Model: "gpt-5", Payload: []byte(body), OriginalRequest: []byte(body)}
			native, _, err := piPrepareConverted(q, false)
			if err != nil {
				t.Fatal(err)
			}
			short := gjson.GetBytes(native.Payload, "tools.0.name").String()
			if short == long || short == "" {
				t.Fatal("fixture did not exercise shortening")
			}
			payload, _ := sjson.Set(piCodecResult, "output.1.name", short)
			payload, _ = sjson.Set(payload, "output.2.name", short)
			cap := &piCodecCapture{response: []byte(payload)}
			res, err := NewPiConvertedExecutor(cap).Execute(context.Background(), "", Credential{}, q)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(res.Payload), long) {
				t.Fatalf("original name lost %s", res.Payload)
			}
			if string(q.Payload) != body || string(q.OriginalRequest) != body {
				t.Fatal("caller bytes mutated")
			}
		})
	}
	q := ExecuteRequest{Format: "openai-response", RequestPath: "/v1/responses", Payload: []byte(`{"input":"unchanged"}`), OriginalRequest: []byte("opaque"), ContinuityKey: "keep"}
	cap := &piCodecCapture{response: []byte("native opaque result")}
	res, err := NewPiConvertedExecutor(cap).Execute(context.Background(), "", Credential{}, q)
	if err != nil || string(res.Payload) != "native opaque result" || string(cap.request.OriginalRequest) != "opaque" || cap.request.ContinuityKey != "keep" {
		t.Fatal("native path was translated")
	}
}

func TestPiConvertedPreflightCommonFieldsAndHeaders(t *testing.T) {
	for _, tc := range []struct{ format, body, header string }{
		{"openai", `{"model":"gpt-5","messages":[{"role":"user","content":"hello"}],"temperature":0.2,"max_tokens":64,"top_p":0.9}`, "Authorization"},
		{"claude", `{"model":"gpt-5","max_tokens":64,"system":"be precise","temperature":0.2,"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}]}]}`, "X-Api-Key"},
		{"gemini", `{"systemInstruction":{"parts":[{"text":"be precise"}]},"generationConfig":{"temperature":0.2,"maxOutputTokens":64},"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`, "X-Goog-Api-Key"},
	} {
		t.Run(tc.format, func(t *testing.T) {
			q := ExecuteRequest{Format: tc.format, Model: "gpt-5", Payload: []byte(tc.body), Headers: http.Header{tc.header: []string{"downstream-only"}}}
			if tc.format == "claude" {
				q.Headers.Set("Anthropic-Version", "2023-06-01")
			}
			for _, stream := range []bool{false, true} {
				if err := ValidatePiConvertedRequest(q, stream); err != nil {
					t.Fatalf("ordinary request rejected %v", err)
				}
			}
			cap := &piCodecCapture{response: []byte(piCodecResult)}
			_, err := NewPiConvertedExecutor(cap).Execute(context.Background(), "", Credential{}, q)
			if err != nil {
				t.Fatal(err)
			}
			if tc.format != "openai" && cap.request.Headers.Get(tc.header) != "" {
				t.Fatal("downstream credential forwarded")
			}
			if cap.request.Headers.Get("Anthropic-Version") != "" {
				t.Fatal("version leaked upstream")
			}
			if q.Headers.Get(tc.header) != "downstream-only" {
				t.Fatal("caller header mutated")
			}
			if tc.format != "openai" {
				first := gjson.GetBytes(cap.request.Payload, "input.0")
				if first.Get("role").String() != "developer" || first.Get("content.0.text").String() != "be precise" {
					t.Fatalf("instruction privilege lost %s", cap.request.Payload)
				}
			}
			q.ConfiguredHeaders = []string{"X-Custom: value"}
			if ValidatePiConvertedRequest(q, false) == nil {
				t.Fatal("configured headers admitted")
			}
		})
	}
}

func TestPiConvertedToolHistory(t *testing.T) {
	bodies := map[string]string{
		"openai": `{"model":"gpt-5","messages":[{"role":"assistant","tool_calls":[{"id":"call_a","type":"function","function":{"name":"lookup","arguments":"{\"x\":1}"}},{"id":"call_b","type":"function","function":{"name":"lookup","arguments":"{\"x\":2}"}}]},{"role":"tool","tool_call_id":"call_b","content":"second"},{"role":"tool","tool_call_id":"call_a","content":"first"}]}`,
		"claude": `{"model":"gpt-5","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_a","name":"lookup","input":{"x":1}},{"type":"tool_use","id":"call_b","name":"lookup","input":{"x":2}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_b","content":"second"},{"type":"tool_result","tool_use_id":"call_a","content":"first"}]}]}`,
		"gemini": `{"contents":[{"role":"model","parts":[{"functionCall":{"id":"call_a","name":"lookup","args":{"x":1}}},{"functionCall":{"id":"call_b","name":"lookup","args":{"x":2}}}]},{"role":"user","parts":[{"functionResponse":{"id":"call_b","name":"lookup","response":{"result":"second"}}},{"functionResponse":{"id":"call_a","name":"lookup","response":{"result":"first"}}}]}]}`,
	}
	for format, body := range bodies {
		t.Run(format, func(t *testing.T) {
			cap := &piCodecCapture{response: []byte(piCodecResult)}
			_, err := NewPiConvertedExecutor(cap).Execute(context.Background(), "", Credential{}, ExecuteRequest{Format: format, Model: "gpt-5", Payload: []byte(body)})
			if err != nil {
				t.Fatal(err)
			}
			items := gjson.GetBytes(cap.request.Payload, "input").Array()
			calls := map[string]string{}
			results := map[string]string{}
			for _, item := range items {
				switch item.Get("type").String() {
				case "function_call":
					calls[item.Get("call_id").String()] = item.Get("arguments").String()
				case "function_call_output":
					results[item.Get("call_id").String()] = item.Get("output").String()
				}
			}
			if len(calls) != 2 || len(results) != 2 || gjson.Get(calls["call_a"], "x").Int() != 1 || gjson.Get(calls["call_b"], "x").Int() != 2 || !strings.Contains(results["call_a"], "first") || !strings.Contains(results["call_b"], "second") {
				t.Fatalf("history linkage %s", cap.request.Payload)
			}
		})
	}
}

func TestPiConvertedParallelToolStream(t *testing.T) {
	for _, tc := range piCodecCases {
		t.Run(tc.format, func(t *testing.T) {
			events := []string{`{"type":"response.created","response":{"id":"resp_fixture","model":"gpt-5","created_at":100}}`}
			for i, id := range []string{"call_a", "call_b"} {
				item := fmt.Sprintf(`{"type":"function_call","id":"fc_%d","call_id":%q,"name":"lookup","arguments":""}`, i, id)
				events = append(events, fmt.Sprintf(`{"type":"response.output_item.added","output_index":%d,"item":%s}`, i, item))
			}
			// Interleaved argument deltas must keep per-item tool identity.
			for _, i := range []int{1, 0} {
				events = append(events, fmt.Sprintf(`{"type":"response.function_call_arguments.delta","output_index":%d,"item_id":"fc_%d","delta":"{\"x\":%d}"}`, i, i, i+1))
			}
			for i, id := range []string{"call_a", "call_b"} {
				events = append(events, fmt.Sprintf(`{"type":"response.output_item.done","output_index":%d,"item":{"type":"function_call","id":"fc_%d","call_id":%q,"name":"lookup","arguments":"{\"x\":%d}"}}`, i, i, id, i+1))
			}
			events = append(events, `{"type":"response.completed","response":`+piCodecResult+`}`)
			cap := &piCodecCapture{}
			for _, event := range events {
				cap.frames = append(cap.frames, piCodecEvent(event))
			}
			res, err := NewPiConvertedExecutor(cap).ExecuteStream(context.Background(), "", Credential{}, ExecuteRequest{Format: tc.format, Model: "gpt-5", Payload: []byte(tc.body)})
			if err != nil {
				t.Fatal(err)
			}
			var output strings.Builder
			var terminal error
			for ch := range res.Chunks {
				if ch.Err != nil {
					terminal = ch.Err
				}
				output.Write(ch.Payload)
			}
			if terminal != nil {
				t.Fatal(terminal)
			}
			wants := []string{"call_a", "call_b", "lookup", "10"}
			if tc.format == "gemini" {
				wants = append(wants, `"x":1`, `"x":2`)
			} else {
				wants = append(wants, `\"x\":1`, `\"x\":2`)
			}
			for _, want := range wants {
				if !strings.Contains(output.String(), want) {
					t.Fatalf("missing %q: %s", want, output.String())
				}
			}
		})
	}
}

func TestPiConvertedRejectAndErrors(t *testing.T) {
	for _, q := range []ExecuteRequest{{Format: "openai-image", Payload: []byte(`{}`)}, {Format: "openai", RequestPath: "/v1/images/generations", Payload: []byte(`{}`)}, {Format: "openai", Payload: []byte(`{`)}, {Format: "openai", Payload: []byte(`{"messages":[],"tools":[{"type":"web_search"}]}`)}} {
		cap := &piCodecCapture{}
		_, err := NewPiConvertedExecutor(cap).Execute(context.Background(), "", Credential{}, q)
		if err == nil || cap.calls != 0 {
			t.Fatal("unsupported request dispatched")
		}
	}
	cap := &piCodecCapture{}
	if _, err := NewPiConvertedExecutor(cap).CountTokens(context.Background(), "", Credential{}, ExecuteRequest{}); err == nil {
		t.Fatal("count supported")
	}
	sentinel := errors.New("native error")
	cap.err = sentinel
	_, err := NewPiConvertedExecutor(cap).Execute(context.Background(), "", Credential{}, ExecuteRequest{Format: "openai", Payload: []byte(piCodecCases[0].body), Model: "gpt-5"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("native error lost: %v", err)
	}
}
func TestPiConvertedCommonImages(t *testing.T) {
	cases := []struct{ format, body string }{
		{"openai", `{"model":"gpt-5","messages":[{"role":"user","content":[{"type":"text","text":"what"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`},
		{"claude", `{"model":"gpt-5","max_tokens":16,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}`},
		{"claude", `{"model":"gpt-5","max_tokens":16,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.test/a.png"}}]}]}`},
		{"gemini", `{"model":"gpt-5","contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"AAAA"}}]}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.format, func(t *testing.T) {
			q := ExecuteRequest{Format: tc.format, Model: "gpt-5", Payload: []byte(tc.body)}
			if err := ValidatePiConvertedRequest(q, false); err != nil {
				t.Fatalf("common image rejected: %v", err)
			}
		})
	}
	for _, tc := range []struct{ format, body string }{
		{"openai", `{"model":"gpt-5","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:application/pdf;base64,AAAA"}}]}]}`},
		{"claude", `{"model":"gpt-5","max_tokens":16,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"AAAA"}}]}]}`},
		{"gemini", `{"model":"gpt-5","contents":[{"role":"user","parts":[{"fileData":{"mimeType":"audio/wav","fileUri":"x"}}]}]}`},
	} {
		if ValidatePiConvertedRequest(ExecuteRequest{Format: tc.format, Model: "gpt-5", Payload: []byte(tc.body)}, false) == nil {
			t.Fatalf("unsupported content admitted: %s", tc.format)
		}
	}
}
func TestPiConvertedStreamNativeErrorAndFailureCancels(t *testing.T) {
	sentinel := piError("pi_upstream_error", "maybe_sent")
	cap := &piCodecCapture{chunkErr: sentinel}
	q := ExecuteRequest{Format: "openai", Model: "gpt-5", Payload: []byte(piCodecCases[0].body)}
	res, err := NewPiConvertedExecutor(cap).ExecuteStream(context.Background(), "", Credential{}, q)
	if err != nil {
		t.Fatal(err)
	}
	got := <-res.Chunks
	if !errors.Is(got.Err, sentinel) {
		t.Fatalf("lost native error %v", got.Err)
	}
	cap = &piCodecCapture{frames: [][]byte{[]byte("data: broken\n\n")}, cancelled: make(chan struct{})}
	res, err = NewPiConvertedExecutor(cap).ExecuteStream(context.Background(), "", Credential{}, q)
	if err != nil {
		t.Fatal(err)
	}
	for range res.Chunks {
	}
	select {
	case <-cap.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("codec error did not cancel native producer")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cap = &piCodecCapture{}
	if _, err := NewPiConvertedExecutor(cap).Execute(ctx, "", Credential{}, q); !errors.Is(err, context.Canceled) || cap.calls != 0 {
		t.Fatal("pre-cancel dispatched")
	}
}

func TestPiConvertedSSEAssembly(t *testing.T) {
	var parser piCodecSSE
	var got []string
	consume := func(data []byte) bool { got = append(got, string(data)); return true }
	input := ": comment\r\nevent: response.output_text.delta\r\ndata: {\"type\":\"response.output_text.delta\",\r\ndata: \"delta\":\"hello\"}\r\n\r\ndata: {\"type\":\"response.completed\"}\n\n"
	if !parser.feed([]byte(input), consume) || len(got) != 2 || !gjson.Valid(got[0]) || gjson.Get(got[0], "delta").String() != "hello" || parser.size != 0 {
		t.Fatalf("SSE assembly %q", got)
	}
}

func TestPiConvertedMalformedStreamAndCancel(t *testing.T) {
	for _, frame := range []string{"data: invalid\n\n", "data: {}\n\n", "data: {\"type\":\"response.output_text.delta\"}", "data: " + strings.Repeat("x", piMaxFrameBytes+1)} {
		cap := &piCodecCapture{frames: [][]byte{[]byte(frame)}}
		res, err := NewPiConvertedExecutor(cap).ExecuteStream(context.Background(), "", Credential{}, ExecuteRequest{Format: "openai", Model: "gpt-5", Payload: []byte(piCodecCases[0].body)})
		if err != nil {
			t.Fatal(err)
		}
		failed := false
		for c := range res.Chunks {
			failed = failed || c.Err != nil
		}
		if !failed {
			t.Fatal("malformed/truncated stream accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cap := &piCodecCapture{cancelled: make(chan struct{})}
	res, err := NewPiConvertedExecutor(cap).ExecuteStream(ctx, "", Credential{}, ExecuteRequest{Format: "openai", Model: "gpt-5", Payload: []byte(piCodecCases[0].body)})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-res.Chunks:
	case <-time.After(2 * time.Second):
		t.Fatal("conversion blocked on cancellation")
	}
	select {
	case <-cap.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("native not cancelled")
	}
}
