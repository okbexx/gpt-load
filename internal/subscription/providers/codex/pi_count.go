// Restricted-text counting semantics adapted from CLIProxyAPI v7.3.6,
// internal/runtime/executor/codex_executor_tokens.go (MIT).
// Copyright (c) 2025-2005.9 Luis Pater
// Copyright (c) 2025.9-present Router-For.ME
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tiktoken-go/tokenizer"
)

// EstimatePiTokens is a bounded, credential-free LOCAL estimate, not Pi inference.
// Only pure request codecs are shared with CPA; no CPA executor, auth, transport,
// configuration, or token-count implementation is invoked here. The codecs are
// registered by the existing embedded package initialization.
//
// Preserve the baseline estimator's deliberately narrow semantics: trimmed
// instructions and translated message text joined by newlines, without role
// overhead or a synthetic default instruction. In particular, Responses items
// without type=message or with string content are not counted by the baseline.
func EstimatePiTokens(ctx context.Context, q ExecuteRequest) (ExecuteResponse, error) {
	if err := ctx.Err(); err != nil {
		return ExecuteResponse{}, err
	}
	if err := ValidatePiLocalTokenCount(q); err != nil {
		return ExecuteResponse{}, err
	}
	body := sdktranslator.TranslateRequest(sdktranslator.FromString(q.Format), sdktranslator.FromString("codex"), q.Model, append([]byte(nil), q.Payload...), false)
	if err := ctx.Err(); err != nil {
		return ExecuteResponse{}, err
	}
	root := gjson.ParseBytes(body)
	segments := make([]string, 0)
	appendText := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			segments = append(segments, s)
		}
	}
	appendText(root.Get("instructions").String())
	for _, item := range root.Get("input").Array() {
		if err := ctx.Err(); err != nil {
			return ExecuteResponse{}, err
		}
		if item.Get("type").String() != "message" {
			continue
		}
		for _, part := range item.Get("content").Array() {
			appendText(part.Get("text").String())
		}
	}
	encoding := tokenizer.Cl100kBase
	model := strings.ToLower(strings.TrimSpace(q.Model))
	if strings.HasPrefix(model, "gpt-5") || strings.HasPrefix(model, "gpt-4.1") || strings.HasPrefix(model, "gpt-4o") {
		encoding = tokenizer.O200kBase
	}
	codec, err := tokenizer.Get(encoding)
	if err != nil {
		return ExecuteResponse{}, fmt.Errorf("pi local tokenizer: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ExecuteResponse{}, err
	}
	count := 0
	if text := strings.Join(segments, "\n"); text != "" {
		count, err = codec.Count(text)
	}
	if err != nil {
		return ExecuteResponse{}, fmt.Errorf("pi local token count: %w", err)
	}
	// Count is synchronous and bounded: cancellation never leaves detached workers.
	if err := ctx.Err(); err != nil {
		return ExecuteResponse{}, err
	}
	var payload []byte
	switch q.Format {
	case "openai-response":
		payload = []byte(fmt.Sprintf(`{"object":"response.input_tokens","input_tokens":%d}`, count))
	case "claude":
		payload = []byte(fmt.Sprintf(`{"input_tokens":%d}`, count))
	case "gemini":
		payload = []byte(fmt.Sprintf(`{"totalTokens":%d,"promptTokensDetails":[{"modality":"TEXT","tokenCount":%d}]}`, count, count))
	}
	return ExecuteResponse{Payload: payload}, nil
}

// ValidatePiLocalTokenCount mirrors the existing restricted-text admission
// contract at the exported helper boundary as well as the provider boundary.
// Transport metadata is immaterial: local estimation never uses it.
func ValidatePiLocalTokenCount(q ExecuteRequest) error {
	bad := errors.New("pi local token count request is unsupported")
	model := strings.ToLower(strings.TrimSpace(q.Model))
	if !(strings.HasPrefix(model, "gpt-5") || strings.HasPrefix(model, "gpt-4") || strings.HasPrefix(model, "gpt-3")) {
		return bad
	}
	if len(q.Payload) > piMaxRequestBytes {
		return bad
	}
	var root map[string]any
	if json.Unmarshal(q.Payload, &root) != nil || root == nil {
		return bad
	}
	switch q.Format {
	case "openai-response":
		if !piCountOnlyFields(root, "model", "input", "instructions") {
			return bad
		}
		if v, ok := root["instructions"]; ok {
			if _, ok := v.(string); !ok {
				return bad
			}
		}
		if root["input"] == nil {
			return nil
		}
		if _, ok := root["input"].(string); ok {
			return nil
		}
		items, ok := root["input"].([]any)
		if !ok {
			return bad
		}
		for _, item := range items {
			m, ok := item.(map[string]any)
			if !ok || !piCountOnlyFields(m, "type", "role", "content") {
				return bad
			}
			if v, ok := m["type"]; ok && v != "message" {
				return bad
			}
			if v, ok := m["role"]; ok {
				if _, ok := v.(string); !ok {
					return bad
				}
			}
			if !piCountTextContent(m["content"], "input_text", "text") {
				return bad
			}
		}
	case "claude":
		if !piCountOnlyFields(root, "model", "system", "messages") {
			return bad
		}
		if v, ok := root["system"]; ok && !piCountTextContent(v, "text") {
			return bad
		}
		if _, ok := root["messages"]; !ok {
			return nil
		}
		items, ok := root["messages"].([]any)
		if !ok {
			return bad
		}
		for _, item := range items {
			m, ok := item.(map[string]any)
			if !ok || !piCountOnlyFields(m, "role", "content") || !piCountTextContent(m["content"], "text") {
				return bad
			}
		}
	case "gemini":
		if !piCountOnlyFields(root, "contents") {
			return bad
		}
		if _, ok := root["contents"]; !ok {
			return nil
		}
		items, ok := root["contents"].([]any)
		if !ok {
			return bad
		}
		for _, item := range items {
			m, ok := item.(map[string]any)
			if !ok || !piCountOnlyFields(m, "role", "parts") {
				return bad
			}
			parts, ok := m["parts"].([]any)
			if !ok {
				return bad
			}
			for _, part := range parts {
				p, ok := part.(map[string]any)
				if !ok || !piCountOnlyFields(p, "text") {
					return bad
				}
				if _, ok := p["text"].(string); !ok {
					return bad
				}
			}
		}
	default:
		return bad
	}
	return nil
}

func piCountOnlyFields(m map[string]any, fields ...string) bool {
	for k := range m {
		found := false
		for _, field := range fields {
			if k == field {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func piCountTextContent(v any, types ...string) bool {
	if _, ok := v.(string); ok {
		return true
	}
	parts, ok := v.([]any)
	if !ok {
		return false
	}
	for _, part := range parts {
		p, ok := part.(map[string]any)
		if !ok || !piCountOnlyFields(p, "type", "text") {
			return false
		}
		if _, ok := p["text"].(string); !ok {
			return false
		}
		kind, ok := p["type"].(string)
		if !ok {
			return false
		}
		found := false
		for _, allowed := range types {
			if kind == allowed {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
