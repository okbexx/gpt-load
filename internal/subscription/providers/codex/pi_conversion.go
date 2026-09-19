package codex

// This file shares CLIProxyAPI's registered wire codecs, NOT its executor or
// transport. The supplied native executor is the sole execution dependency.
import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"

	codec "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator/builtin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type piConvertedExecutor struct{ native Executor }

// NewPiConvertedExecutor adds isolated chat wire conversion to a Pi native
// executor. Production callers MUST supply NewPiExecutor, never NewExecutor.
// It is deliberately not installed by this constructor in any provider router.
func NewPiConvertedExecutor(native Executor) Executor { return &piConvertedExecutor{native: native} }

// ValidatePiConvertedRequest is a pure preflight boundary: no credential,
// executor, socket or transport is consulted. Call after existing converted
// fidelity preparation and before credential selection/dispatch.
func ValidatePiConvertedRequest(q ExecuteRequest, stream bool) error {
	if q.Format == "openai-response" {
		return ValidatePiRequest(q)
	}
	_, _, err := piPrepareConverted(q, stream)
	return err
}

func piPrepareConverted(q ExecuteRequest, stream bool) (ExecuteRequest, []byte, error) {
	bad := func() (ExecuteRequest, []byte, error) {
		return q, nil, piError("pi_conversion_unsupported", "not_sent")
	}
	switch q.Format {
	case "openai":
		if q.RequestPath != "" && q.RequestPath != "/v1/chat/completions" {
			return bad()
		}
	case "claude":
		if q.RequestPath != "" && q.RequestPath != "/v1/messages" {
			return bad()
		}
	case "gemini":
		if q.RequestPath != "" {
			suffix := ":generateContent"
			if stream {
				suffix = ":streamGenerateContent"
			}
			if q.RequestPath != "/v1beta/models/"+q.Model+suffix && q.RequestPath != "/v1/models/"+q.Model+suffix {
				return bad()
			}
		}
	default:
		return bad()
	}
	if len(q.Payload) > piMaxRequestBytes || !json.Valid(q.Payload) || !gjson.ParseBytes(q.Payload).IsObject() {
		return q, nil, piError("pi_invalid_request", "not_sent")
	}
	if v := gjson.GetBytes(q.Payload, "stream"); v.Exists() && (v.Type != gjson.True && v.Type != gjson.False || v.Bool() != stream) {
		return q, nil, piError("pi_stream_mismatch", "not_sent")
	}
	if v := gjson.GetBytes(q.Payload, "model"); v.Exists() && (v.Type != gjson.String || v.String() != q.Model) {
		return q, nil, piError("pi_model_mismatch", "not_sent")
	}
	if !piConvertedFieldsSupported(q) {
		return bad()
	}
	registry := builtin.Registry()
	from := codec.FromString(q.Format)
	// Fail closed rather than accepting the registry's identity fallback. Plugins
	// are intentionally outside this pure-codec contract.
	if registry.HasPluginHooks() || !registry.HasRequestTransformer(from, codec.FormatCodex) || !registry.HasStreamResponseTransformer(from, codec.FormatCodex) || !registry.HasNonStreamResponseTransformer(from, codec.FormatCodex) {
		return bad()
	}
	original := append([]byte(nil), q.OriginalRequest...)
	if len(original) == 0 {
		original = append([]byte(nil), q.Payload...)
	}
	if len(original) > piMaxRequestBytes || !json.Valid(original) {
		return q, nil, piError("pi_invalid_request", "not_sent")
	}
	source := append([]byte(nil), q.Payload...)
	// Preserve the existing adapter's mid-conversation instruction boundary even
	// when this seam is exercised directly (adapter may already have done this).
	if q.Format == "claude" {
		for i, m := range gjson.GetBytes(source, "messages").Array() {
			if m.Get("role").String() == "system" {
				source, _ = sjson.SetBytes(source, "messages."+strconv.Itoa(i)+".role", "developer")
			}
		}
	}
	payload := registry.TranslateRequest(from, codec.FormatCodex, q.Model, source, stream)
	// Claude/Gemini codecs force stream=true for CPA's HTTP transport. Pi owns
	// that transport and its unary bridge aggregates the terminal native event.
	payload, err := sjson.SetBytes(payload, "stream", stream)
	if err != nil {
		return bad()
	}
	// Client authentication/version metadata belongs to the downstream protocol,
	// not the Codex upstream. Clone before stripping; never mutate caller headers.
	q.Headers = q.Headers.Clone()
	for key := range q.Headers {
		switch strings.ToLower(key) {
		case "x-api-key", "anthropic-version", "x-goog-api-key":
			delete(q.Headers, key)
		}
	}
	q.Format = "openai-response"
	q.RequestPath = "/v1/responses"
	q.Payload = payload
	q.OriginalRequest = append([]byte(nil), payload...)
	if err := ValidatePiRequest(q); err != nil {
		return q, nil, err
	}
	return q, original, nil
}

// Keep baseline generation controls admissible; some are deliberate Codex
// codec no-ops, documented in converted-contract.md. Only genuinely unsupported
// operation/content families are rejected here. Adapter fidelity preparation
// (including allowlist normalization) must still run before this seam.
func piConvertedFieldsSupported(q ExecuteRequest) bool {
	ok := true
	root := gjson.ParseBytes(q.Payload)
	for _, key := range []string{"previous_response_id", "conversation", "background", "store"} {
		if v := root.Get(key); v.Exists() && v.Type != gjson.Null && v.Type != gjson.False {
			return false
		}
	}
	for _, tool := range root.Get("tools").Array() {
		switch q.Format {
		case "openai":
			if tool.Get("type").String() != "function" {
				return false
			}
		case "claude":
			if tool.Get("type").Exists() && tool.Get("type").String() != "custom" {
				return false
			}
		case "gemini":
			tool.ForEach(func(k, v gjson.Result) bool { ok = k.String() == "functionDeclarations"; return ok })
			if !ok {
				return false
			}
		}
	}
	if c := root.Get("tool_choice"); c.IsObject() && c.Get("type").String() == "allowed_tools" {
		return false
	} // adapter must prepare allowlist first
	// Reject multimodal/search/history blocks not covered by this slice.
	var walk func(gjson.Result) bool
	walk = func(v gjson.Result) bool {
		if v.IsArray() {
			for _, x := range v.Array() {
				if !walk(x) {
					return false
				}
			}
			return true
		}
		if !v.IsObject() {
			return true
		}
		typ := v.Get("type").String()
		if typ == "image_url" {
			iv := v.Get("image_url")
			url := iv.Get("url").String()
			return q.Format == "openai" && (strings.HasPrefix(url, "data:image/") || strings.HasPrefix(url, "http"))
		}
		if typ == "image" {
			s := v.Get("source")
			return q.Format == "claude" && s.IsObject() && (s.Get("type").String() == "base64" || s.Get("type").String() == "url")
		}
		if typ == "document" {
			return false
		}
		valid := true
		v.ForEach(func(k, x gjson.Result) bool {
			switch k.String() {
			case "cache_control":
				// Foreign prompt-cache hints are admitted with baseline no-op
				// semantics; they are not multimodal content discriminators.
				valid = true
			case "image_url":
				url := v.Get("url").String()
				if url == "" {
					url = v.Get("url.url").String()
				}
				valid = q.Format == "openai" && strings.HasPrefix(url, "data:image/") || q.Format == "openai" && strings.HasPrefix(url, "http")
			case "image":
				valid = q.Format == "claude" && v.IsObject() && (v.Get("source.type").String() == "base64" || v.Get("source.type").String() == "url")
			case "inlineData", "inline_data":
				valid = q.Format == "gemini" && v.IsObject() && v.Get("data").Type == gjson.String && v.Get("mimeType").Type == gjson.String
			case "fileData", "file_data", "audio", "document":
				valid = false
			case "type":
				switch x.String() {
				case "text", "input_text", "output_text", "tool_use", "tool_result", "function", "thinking", "redacted_thinking", "image", "image_url":
				default:
					valid = false
				}
			default:
				valid = walk(x)
			}
			return valid
		})
		return valid
	}
	// Only inspect message content, not arbitrary user tool arguments/schema.
	for _, m := range root.Get("messages").Array() {
		c := m.Get("content")
		if c.IsArray() {
			for _, b := range c.Array() {
				typ := b.Get("type").String()
				if typ == "tool_use" {
					continue
				}
				if typ == "tool_result" {
					if !walk(b.Get("content")) {
						return false
					}
					continue
				}
				if !walk(b) {
					return false
				}
			}
		}
	}
	for _, m := range root.Get("contents").Array() {
		for _, p := range m.Get("parts").Array() {
			if p.Get("inlineData").Exists() || p.Get("inline_data").Exists() {
				continue
			}
			if p.Get("functionCall").Exists() || p.Get("functionResponse").Exists() {
				continue
			}
			if !walk(p) {
				return false
			}
		}
	}
	return true
}

func (e *piConvertedExecutor) CountTokens(context.Context, string, Credential, ExecuteRequest) (ExecuteResponse, error) {
	return ExecuteResponse{}, piError("pi_unsupported_capability", "not_sent")
}
func (e *piConvertedExecutor) Execute(ctx context.Context, id string, c Credential, q ExecuteRequest) (ExecuteResponse, error) {
	if e.native == nil {
		return ExecuteResponse{}, piError("pi_driver_unavailable", "not_sent")
	}
	if q.Format == "openai-response" {
		return e.native.Execute(ctx, id, c, q)
	}
	if err := ctx.Err(); err != nil {
		return ExecuteResponse{}, err
	}
	native, original, err := piPrepareConverted(q, false)
	if err != nil {
		return ExecuteResponse{}, err
	}
	res, err := e.native.Execute(ctx, id, c, native)
	if err != nil {
		return res, err
	}
	if !json.Valid(res.Payload) || len(res.Payload) > piMaxFrameBytes || gjson.GetBytes(res.Payload, "status").String() != "completed" {
		res.Payload = nil
		return res, piError("pi_conversion_invalid_response", "maybe_sent")
	}
	event := append([]byte(`{"type":"response.completed","response":`), res.Payload...)
	event = append(event, '}')
	var state any
	res.Payload = builtin.Registry().TranslateNonStream(ctx, codec.FormatCodex, codec.FromString(q.Format), q.Model, original, native.Payload, event, &state)
	if !json.Valid(res.Payload) {
		res.Payload = nil
		return res, piError("pi_conversion_invalid_response", "maybe_sent")
	}
	return res, nil
}
func (e *piConvertedExecutor) ExecuteStream(ctx context.Context, id string, c Credential, q ExecuteRequest) (*ExecuteStreamResponse, error) {
	if e.native == nil {
		return nil, piError("pi_driver_unavailable", "not_sent")
	}
	if q.Format == "openai-response" {
		return e.native.ExecuteStream(ctx, id, c, q)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	native, original, err := piPrepareConverted(q, true)
	if err != nil {
		return nil, err
	}
	child, cancel := context.WithCancel(ctx)
	res, err := e.native.ExecuteStream(child, id, c, native)
	if err != nil {
		cancel()
		return res, err
	}
	if res == nil {
		cancel()
		return nil, piError("pi_conversion_invalid_response", "maybe_sent")
	}
	if res.Chunks == nil {
		cancel()
		return res, piError("pi_conversion_invalid_response", "maybe_sent")
	}
	out := make(chan ExecuteStreamChunk, 1)
	result := *res
	result.Chunks = out
	go func() {
		defer close(out)
		defer cancel()
		send := func(ch ExecuteStreamChunk) bool {
			select {
			case out <- ch:
				return true
			case <-ctx.Done():
				return false
			}
		}
		fail := func() { send(ExecuteStreamChunk{Err: piError("pi_conversion_invalid_stream", "maybe_sent")}) }
		var state any
		var parser piCodecSSE
		completed := false
		geminiState := &piGeminiStream{model: q.Model, original: original}
		consume := func(data []byte) bool {
			if bytes.Equal(data, []byte("[DONE]")) {
				return completed
			}
			if completed || !json.Valid(data) {
				return false
			}
			typ := gjson.GetBytes(data, "type").String()
			if !strings.HasPrefix(typ, "response.") {
				return false
			}
			if typ == "response.failed" || typ == "response.incomplete" || typ == "response.error" {
				return false
			}
			if typ == "response.completed" {
				if gjson.GetBytes(data, "response.status").String() != "completed" {
					return false
				}
				completed = true
			}
			var chunks [][]byte
			if q.Format == "gemini" {
				chunks = piGeminiEvent(child, q.Model, original, data, geminiState)
			} else {
				chunks = builtin.Registry().TranslateStream(child, codec.FormatCodex, codec.FromString(q.Format), q.Model, original, native.Payload, append([]byte("data: "), data...), &state)
			}
			for _, b := range chunks {
				if len(b) > 0 && !send(ExecuteStreamChunk{Payload: append([]byte(nil), b...)}) {
					return false
				}
			}
			return true
		}
		for {
			select {
			case <-ctx.Done():
				return
			case ch, ok := <-res.Chunks:
				if !ok {
					if parser.size != 0 || !completed {
						fail()
					}
					return
				}
				if ch.Err != nil {
					send(ExecuteStreamChunk{Err: ch.Err})
					return
				}
				if !parser.feed(ch.Payload, consume) {
					fail()
					return
				}
			}
		}
	}()
	return &result, nil
}

// SSE is assembled across arbitrary native chunks. Per-event memory is bounded
// independently of transport-frame bounds. CRLF and multi-data-line events are
// normalized before passing a single data: line to the baseline codec.
type piCodecSSE struct {
	line, data []byte
	size       int
}

func (p *piCodecSSE) feed(chunk []byte, consume func([]byte) bool) bool {
	for _, b := range chunk {
		p.size++
		if p.size > piMaxFrameBytes {
			return false
		}
		if b != '\n' {
			p.line = append(p.line, b)
			continue
		}
		line := bytes.TrimSuffix(p.line, []byte{'\r'})
		p.line = nil
		if len(line) == 0 {
			if len(p.data) > 0 {
				data := bytes.TrimSuffix(p.data, []byte{'\n'})
				if !consume(data) {
					return false
				}
			}
			p.data = nil
			p.size = 0
			continue
		}
		if line[0] == ':' {
			continue
		}
		field, value, found := bytes.Cut(line, []byte{':'})
		if !found {
			return false
		}
		value = bytes.TrimPrefix(value, []byte{' '})
		switch string(field) {
		case "data":
			p.data = append(p.data, value...)
			p.data = append(p.data, '\n')
		case "event", "id", "retry":
		default:
			return false
		}
	}
	return true
}
