package codex

// This is an independent Gemini stream response codec for the Pi wrapper. It
// exists because the pinned shared codec stores only one pending function call.
// It deliberately owns no transport or executor state.
import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type piGeminiStream struct {
	model    string
	id       string
	created  int64
	original []byte
	// Responses output_index identifies the item, not a tool declaration.
	// Track text per content part so one streamed message cannot suppress an
	// independent final-only message (or another part of the same message).
	textEmitted map[[2]int64]bool
}

func piGeminiEvent(ctx context.Context, model string, original, data []byte, state *piGeminiStream) [][]byte {
	if ctx.Err() != nil {
		return nil
	}
	if state == nil {
		return nil
	}
	if state.textEmitted == nil {
		state.textEmitted = make(map[[2]int64]bool)
	}
	root := gjson.ParseBytes(data)
	if !root.IsObject() {
		return nil
	}
	typ := root.Get("type").String()
	base := []byte(`{"candidates":[{"content":{"role":"model","parts":[]}}],"usageMetadata":{"trafficType":"PROVISIONED_THROUGHPUT"},"modelVersion":"","createTime":"","responseId":""}`)
	base, _ = sjson.SetBytes(base, "modelVersion", model)
	if state.id != "" {
		base, _ = sjson.SetBytes(base, "responseId", state.id)
	}
	if state.created != 0 {
		base, _ = sjson.SetBytes(base, "createTime", time.Unix(state.created, 0).Format(time.RFC3339Nano))
	}
	switch typ {
	case "response.created":
		state.id = root.Get("response.id").String()
		state.created = root.Get("response.created_at").Int()
		base, _ = sjson.SetBytes(base, "responseId", state.id)
		base, _ = sjson.SetBytes(base, "createTime", time.Unix(state.created, 0).Format(time.RFC3339Nano))
		return [][]byte{base}
	case "response.output_text.delta":
		key := [2]int64{root.Get("output_index").Int(), root.Get("content_index").Int()}
		// Empty deltas do not establish that a part was streamed.
		if delta := root.Get("delta").String(); delta != "" {
			state.textEmitted[key] = true
		}
		part := []byte(`{"text":""}`)
		part, _ = sjson.SetBytes(part, "text", root.Get("delta").String())
		base, _ = sjson.SetRawBytes(base, "candidates.0.content.parts", []byte(`[{}]`))
		base, _ = sjson.SetRawBytes(base, "candidates.0.content.parts.0", part)
		return [][]byte{base}
	case "response.reasoning_summary_text.delta":
		part := []byte(`{"thought":true,"text":""}`)
		part, _ = sjson.SetBytes(part, "text", root.Get("delta").String())
		base, _ = sjson.SetRawBytes(base, "candidates.0.content.parts.0", part)
		return [][]byte{base}
	case "response.output_item.done":
		item := root.Get("item")
		if item.Get("type").String() == "message" {
			var chunks [][]byte
			for i, content := range item.Get("content").Array() {
				if content.Get("type").String() != "output_text" {
					continue
				}
				text := content.Get("text").String()
				key := [2]int64{root.Get("output_index").Int(), int64(i)}
				if text == "" || state.textEmitted[key] {
					continue
				}
				state.textEmitted[key] = true
				part := []byte(`{"text":""}`)
				part, _ = sjson.SetBytes(part, "text", text)
				chunks = append(chunks, part)
			}
			if len(chunks) == 0 {
				return nil
			}
			base, _ = sjson.SetRawBytes(base, "candidates.0.content.parts", []byte(`[{}]`))
			for i, part := range chunks {
				base, _ = sjson.SetRawBytes(base, "candidates.0.content.parts."+strconv.Itoa(i), part)
			}
			return [][]byte{base}
		}
		if item.Get("type").String() != "function_call" {
			return nil
		}
		name := item.Get("name").String()
		name = piRestoreToolName(original, name)
		part := []byte(`{"functionCall":{"name":"","args":{}}}`)
		part, _ = sjson.SetBytes(part, "functionCall.name", name)
		args := item.Get("arguments").String()
		if gjson.Parse(args).IsObject() {
			part, _ = sjson.SetRawBytes(part, "functionCall.args", []byte(args))
		}
		if id := item.Get("call_id").String(); id != "" {
			part, _ = sjson.SetBytes(part, "functionCall.id", id)
		} else if id := item.Get("id").String(); id != "" {
			part, _ = sjson.SetBytes(part, "functionCall.id", id)
		}
		base, _ = sjson.SetRawBytes(base, "candidates.0.content.parts.0", part)
		return [][]byte{base}
	case "response.completed", "response.incomplete":
		r := root.Get("response")
		in, out := r.Get("usage.input_tokens").Int(), r.Get("usage.output_tokens").Int()
		base, _ = sjson.SetBytes(base, "usageMetadata.promptTokenCount", in)
		base, _ = sjson.SetBytes(base, "usageMetadata.candidatesTokenCount", out)
		base, _ = sjson.SetBytes(base, "usageMetadata.totalTokenCount", in+out)
		if typ == "response.incomplete" {
			base, _ = sjson.SetBytes(base, "candidates.0.finishReason", "MAX_TOKENS")
		} else {
			base, _ = sjson.SetBytes(base, "candidates.0.finishReason", "STOP")
		}
		return [][]byte{base}
	}
	return nil
}

func piRestoreToolName(original []byte, short string) string {
	tools := gjson.GetBytes(original, "tools")
	if !tools.IsArray() {
		return short
	}
	names := make([]string, 0, 4)
	for _, tool := range tools.Array() {
		declarations := tool.Get("functionDeclarations")
		if !declarations.IsArray() {
			continue
		}
		for _, declaration := range declarations.Array() {
			if name := declaration.Get("name"); name.Exists() {
				names = append(names, name.String())
			}
		}
	}
	if len(names) == 0 {
		return short
	}
	// This is the exact inverse of the pinned baseline's ordered shortening
	// algorithm. output_index is an output-item index and must not index tools.
	originalToShort := make(map[string]string, len(names))
	used := make(map[string]struct{}, len(names))
	for _, name := range names {
		candidate := piGeminiShortName(name)
		if _, exists := used[candidate]; exists {
			base := candidate
			for n := 1; ; n++ {
				suffix := "_" + strconv.Itoa(n)
				limit := 64 - len(suffix)
				if len(base) > limit {
					base = base[:limit]
				}
				candidate = base + suffix
				if _, exists := used[candidate]; !exists {
					break
				}
			}
		}
		used[candidate] = struct{}{}
		originalToShort[name] = candidate
	}
	// Reversing only the final mapping also matches duplicate declarations:
	// earlier candidates stay reserved but are no longer aliases of that name.
	for original, candidate := range originalToShort {
		if candidate == short {
			return original
		}
	}
	return short
}

func piGeminiShortName(name string) string {
	const limit = 64
	if len(name) <= limit {
		return name
	}
	if strings.HasPrefix(name, "mcp__") {
		if split := strings.LastIndex(name, "__"); split > 0 {
			name = "mcp__" + name[split+2:]
		}
	}
	if len(name) > limit {
		name = name[:limit]
	}
	return name
}
