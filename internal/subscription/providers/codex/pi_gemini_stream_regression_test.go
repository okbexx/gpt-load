package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func piGeminiRegressionRequest(t *testing.T, names []string) []byte {
	t.Helper()
	declarations := make([]map[string]any, 0, len(names))
	for _, name := range names {
		declarations = append(declarations, map[string]any{"name": name, "parameters": map[string]any{"type": "object"}})
	}
	body, err := json.Marshal(map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}}}, "tools": []any{map[string]any{"functionDeclarations": declarations}}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// Exercise the same fixtures both directly and through conversion, SSE assembly,
// native dispatch and the stream goroutine. Native transport stays local.
func piGeminiRegressionRun(t *testing.T, wrapper bool, body []byte, events []string) []gjson.Result {
	t.Helper()
	var chunks [][]byte
	if wrapper {
		capture := &piCodecCapture{}
		for _, event := range events {
			// Splitting on every byte also exercises CRLF and UTF-8 boundaries.
			for _, b := range piCodecEvent(event) {
				capture.frames = append(capture.frames, []byte{b})
			}
		}
		res, err := NewPiConvertedExecutor(capture).ExecuteStream(context.Background(), "fixture", Credential{}, ExecuteRequest{Format: "gemini", Model: "gpt-5", Payload: body})
		if err != nil {
			t.Fatal(err)
		}
		for chunk := range res.Chunks {
			if chunk.Err != nil {
				t.Fatal(chunk.Err)
			}
			chunks = append(chunks, chunk.Payload)
		}
		if capture.calls != 1 || capture.request.Format != "openai-response" || !gjson.GetBytes(capture.request.Payload, "stream").Bool() {
			t.Fatalf("wrong native dispatch: %+v", capture.request)
		}
	} else {
		state := &piGeminiStream{}
		for _, event := range events {
			chunks = append(chunks, piGeminiEvent(context.Background(), "gpt-5", body, []byte(event), state)...)
		}
	}
	var parts []gjson.Result
	for _, chunk := range chunks {
		if !gjson.ValidBytes(chunk) {
			t.Fatalf("invalid JSON: %s", chunk)
		}
		parts = append(parts, gjson.GetBytes(chunk, "candidates.0.content.parts").Array()...)
	}
	if len(chunks) == 0 || gjson.GetBytes(chunks[len(chunks)-1], "candidates.0.finishReason").String() != "STOP" {
		t.Fatal("missing terminal finish")
	}
	return parts
}

const piGeminiRegressionCompleted = `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":4}}}`

func TestPiGeminiRegressionToolIdentity(t *testing.T) {
	longA, longB := strings.Repeat("a", 64)+"first", strings.Repeat("a", 64)+"second"
	mcpA, mcpB := "mcp__"+strings.Repeat("server_a", 10)+"__lookup", "mcp__"+strings.Repeat("server_b", 10)+"__lookup"
	for _, tc := range []struct {
		name                     string
		declarations, wire, want []string
		indexes                  []int
	}{
		{"second_declaration_at_zero", []string{"alpha", "beta"}, []string{"beta"}, []string{"beta"}, []int{0}},
		{"shifted_repeated_reordered_parallel", []string{"alpha", "beta", "gamma", "delta"}, []string{"beta", "beta", "alpha", "gamma"}, []string{"beta", "beta", "alpha", "gamma"}, []int{3, 2, 5, 4}},
		{"unknown_single_declaration", []string{"alpha"}, []string{"not_declared"}, []string{"not_declared"}, []int{0}},
		{"unknown_multiple_declarations", []string{"alpha", "beta"}, []string{"not_declared"}, []string{"not_declared"}, []int{1}},
		{"long_collisions", []string{longA, longB}, []string{strings.Repeat("a", 62) + "_1", strings.Repeat("a", 64), strings.Repeat("a", 62) + "_1"}, []string{longB, longA, longB}, []int{0, 2, 1}},
		{"mcp_collisions", []string{mcpA, mcpB}, []string{"mcp__lookup_1", "mcp__lookup"}, []string{mcpB, mcpA}, []int{0, 1}},
		// The pinned algorithm reserves names in declaration order (even duplicate
		// originals), then reverses its final original-to-short map.
		{"duplicate_declarations", []string{longA, longA, longB}, []string{strings.Repeat("a", 62) + "_2", strings.Repeat("a", 62) + "_1", strings.Repeat("a", 64)}, []string{longB, longA, strings.Repeat("a", 64)}, []int{0, 1, 2}},
		{"short_name_collision", []string{mcpA, "mcp__lookup"}, []string{"mcp__lookup_1", "mcp__lookup"}, []string{"mcp__lookup", mcpA}, []int{0, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := piGeminiRegressionRequest(t, tc.declarations)
			events := []string{`{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"thinking"}`, `{"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"intro","content":[]}}`}
			for i, index := range tc.indexes {
				events = append(events, fmt.Sprintf(`{"type":"response.output_item.added","output_index":%d,"item":{"type":"function_call","id":"fc_%d","call_id":"call_%d","name":%q,"arguments":""}}`, index, i, i, tc.wire[i]))
			}
			for i := len(tc.indexes) - 1; i >= 0; i-- {
				events = append(events, fmt.Sprintf(`{"type":"response.function_call_arguments.delta","output_index":%d,"item_id":"fc_%d","delta":"{\"x\":%d}"}`, tc.indexes[i], i, i))
			}
			for i, index := range tc.indexes {
				events = append(events, fmt.Sprintf(`{"type":"response.output_item.done","output_index":%d,"item":{"type":"function_call","id":"fc_%d","call_id":"call_%d","name":%q,"arguments":"{\"x\":%d}"}}`, index, i, i, tc.wire[i], i))
			}
			events = append(events, piGeminiRegressionCompleted)
			for _, wrapper := range []bool{false, true} {
				t.Run(fmt.Sprintf("wrapper=%t", wrapper), func(t *testing.T) {
					var names, ids []string
					var args []int64
					for _, part := range piGeminiRegressionRun(t, wrapper, body, events) {
						if call := part.Get("functionCall"); call.Exists() {
							names = append(names, call.Get("name").String())
							ids = append(ids, call.Get("id").String())
							args = append(args, call.Get("args.x").Int())
						}
					}
					if !reflect.DeepEqual(names, tc.want) {
						t.Errorf("tool identity: got %q want %q", names, tc.want)
					}
					if len(ids) != len(tc.wire) {
						t.Fatalf("parallel calls lost: %q", ids)
					}
					for i, id := range ids {
						if id != fmt.Sprintf("call_%d", i) || args[i] != int64(i) {
							t.Errorf("call linkage %d: id=%s args=%d", i, id, args[i])
						}
					}
				})
			}
		})
	}
}

func TestPiGeminiRegressionShorteningMatchesPinnedRequest(t *testing.T) {
	names := []string{strings.Repeat("a", 64) + "first", strings.Repeat("a", 64) + "first", strings.Repeat("a", 64) + "second", "mcp__" + strings.Repeat("server", 12) + "__lookup", "mcp__lookup", "alpha"}
	body := piGeminiRegressionRequest(t, names)
	native, _, err := piPrepareConverted(ExecuteRequest{Format: "gemini", Model: "gpt-5", Payload: body}, true)
	if err != nil {
		t.Fatal(err)
	}
	tools := gjson.GetBytes(native.Payload, "tools").Array()
	if len(tools) != len(names) {
		t.Fatalf("wrong fixture tool count: %s", native.Payload)
	}
	for i, tool := range tools {
		event := fmt.Sprintf(`{"type":"response.output_item.done","output_index":99,"item":{"type":"function_call","name":%q,"arguments":"{}"}}`, tool.Get("name").String())
		parts := piGeminiRegressionRun(t, false, body, []string{event, piGeminiRegressionCompleted})
		if len(parts) != 1 || parts[0].Get("functionCall.name").String() != names[i] {
			t.Errorf("pinned request name %q not restored to %q: %v", tool.Get("name").String(), names[i], parts)
		}
	}
}

func TestPiGeminiRegressionFinalText(t *testing.T) {
	delta := func(index, part int, text string) string {
		return fmt.Sprintf(`{"type":"response.output_text.delta","output_index":%d,"content_index":%d,"item_id":"msg_%d","delta":%q}`, index, part, index, text)
	}
	done := func(index int, texts ...string) string {
		content := make([]map[string]string, 0, len(texts))
		for _, text := range texts {
			content = append(content, map[string]string{"type": "output_text", "text": text})
		}
		b, err := json.Marshal(content)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf(`{"type":"response.output_item.done","output_index":%d,"item":{"id":"msg_%d","type":"message","content":%s}}`, index, index, b)
	}
	for _, tc := range []struct {
		name         string
		events, want []string
	}{
		{"final_only", []string{done(0, "hello")}, []string{"hello"}},
		{"normal_delta_done", []string{delta(0, 0, "hel"), delta(0, 0, "lo"), done(0, "hello")}, []string{"hel", "lo"}},
		{"multiple_final_parts", []string{done(2, "hello", " 世界", "")}, []string{"hello", " 世界"}},
		{"mixed_parts", []string{delta(2, 0, "hello"), done(2, "hello", " world")}, []string{"hello", " world"}},
		{"later_part_delta", []string{delta(2, 1, "world"), done(2, "hello", "world")}, []string{"world", "hello"}},
		{"mixed_messages", []string{delta(0, 0, "first"), done(0, "first"), done(2, "second"), delta(3, 0, "third"), done(3, "third"), done(4, "fourth")}, []string{"first", "second", "third", "fourth"}},
		{"same_text_independent_messages", []string{done(0, "hello"), done(1, "hello")}, []string{"hello", "hello"}},
		{"interleaved_messages", []string{delta(2, 0, "first"), delta(3, 1, "third"), done(2, "first", "second"), done(3, "fourth", "third")}, []string{"first", "third", "second", "fourth"}},
		{"empty_delta", []string{delta(0, 0, ""), done(0, "hello")}, []string{"hello"}},
		{"repeated_done", []string{done(0, "hello"), done(0, "hello")}, []string{"hello"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, wrapper := range []bool{false, true} {
				t.Run(fmt.Sprintf("wrapper=%t", wrapper), func(t *testing.T) {
					events := append(append([]string{}, tc.events...), piGeminiRegressionCompleted)
					var texts []string
					for _, part := range piGeminiRegressionRun(t, wrapper, piGeminiRegressionRequest(t, nil), events) {
						if text := part.Get("text").String(); text != "" && !part.Get("thought").Bool() {
							texts = append(texts, text)
						}
					}
					if !reflect.DeepEqual(texts, tc.want) {
						t.Errorf("text parts: got %q want %q", texts, tc.want)
					}
				})
			}
		})
	}
}
