//go:build pi_integration

package piintegration_test

// All upstream responses, signatures, tokens and account identities below are
// synthetic local fixtures. The bridge and pinned pi-ai transport are real.
import (
	"bytes"
	"context"
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
	"gpt-load/internal/subscription/providers/codex"
)

func contractRequest(base string) codex.ExecuteRequest {
	return codex.ExecuteRequest{Model: "gpt-5.4", Format: "openai-response", RequestPath: "/v1/responses", BaseURL: base,
		Payload: json.RawMessage(`{"model":"gpt-5.4","input":[{"role":"user","content":"synthetic contract fixture"}],"store":false}`)}
}
func contractCredential() codex.Credential {
	return codex.Credential{AccessToken: fixtureToken("acct-synthetic-contract"), AccountID: "acct-synthetic-contract"}
}
func fixtureSSE(events ...map[string]any) []byte {
	var wire bytes.Buffer
	for _, event := range events {
		b, err := json.Marshal(event)
		if err != nil {
			panic(err)
		}
		fmt.Fprintf(&wire, "event: %s\ndata: %s\n\n", event["type"], b)
	}
	return wire.Bytes()
}
func createdEvent() map[string]any {
	return map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_synthetic", "status": "in_progress", "output": []any{}}}
}

// Observe all chunks through channel closure, including an error delivered after
// successful headers/partial content. Context cancellation also bounds failures.
func runContract(t *testing.T, bridge codex.Executor, q codex.ExecuteRequest, streaming bool) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !streaming {
		out, err := bridge.Execute(ctx, "synthetic", contractCredential(), q)
		return out.Payload, err
	}
	out, err := bridge.ExecuteStream(ctx, "synthetic", contractCredential(), q)
	if err != nil {
		return nil, err
	}
	var wire bytes.Buffer
	var terminalErr error
	for {
		select {
		case chunk, ok := <-out.Chunks:
			if !ok {
				return wire.Bytes(), terminalErr
			}
			if terminalErr != nil {
				t.Error("stream delivered a chunk after its error")
			}
			if chunk.Err != nil {
				terminalErr = chunk.Err
			}
			wire.Write(chunk.Payload)
		case <-ctx.Done():
			t.Fatal("stream did not close before fixture deadline")
			return nil, ctx.Err()
		}
	}
}
func requirePiError(t *testing.T, err error, dispatch string, status int) {
	t.Helper()
	var pe *codex.PiError
	if !errors.As(err, &pe) {
		t.Fatalf("want PiError, got %T: %v", err, err)
	}
	if pe.DispatchState() != dispatch || pe.StatusCode() != status {
		t.Fatalf("error dispatch/status = %s/%d, want %s/%d: %v", pe.DispatchState(), pe.StatusCode(), dispatch, status, err)
	}
	if pe.ErrorType() != "pi_driver_error" || !strings.HasPrefix(pe.ErrorCode(), "pi_") {
		t.Fatalf("unexpected error contract: %v", pe)
	}
	for _, secret := range []string{contractCredential().AccessToken, "synthetic-local-bridge-secret-for-integration", "SYNTHETIC_PRIVATE_UPSTREAM_DETAIL"} {
		if strings.Contains(fmt.Sprintf("%s %s %s", err, pe.ErrorCode(), pe.ConversionCode()), secret) {
			t.Error("error leaked synthetic sensitive data")
		}
	}
}

func TestRealPiReasoningSignatureAndCachedTokenFidelity(t *testing.T) {
	reasoning := map[string]any{"id": "rs_synthetic", "type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "Synthetic reasoning: 雪"}}, "encrypted_content": "synthetic-signature+/==", "fixture_reasoning_extension": map[string]any{"opaque": true}}
	message := map[string]any{"id": "msg_synthetic", "type": "message", "role": "assistant", "status": "completed", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": "Synthetic answer ✓", "annotations": []any{}, "logprobs": []any{}}}}
	terminal := map[string]any{"id": "resp_synthetic", "object": "response", "status": "completed", "model": "gpt-5.4", "output": []any{reasoning, message}, "service_tier": "default", "error": nil, "incomplete_details": nil,
		"usage":             map[string]any{"input_tokens": 101, "output_tokens": 23, "total_tokens": 124, "input_tokens_details": map[string]any{"cached_tokens": 73, "cache_write_tokens": 11, "fixture_detail": 9}, "output_tokens_details": map[string]any{"reasoning_tokens": 17, "fixture_detail": 4}},
		"fixture_extension": map[string]any{"nested": []any{nil, false, "opaque"}}}
	wire := fixtureSSE(createdEvent(),
		map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "rs_synthetic", "type": "reasoning", "summary": []any{}}},
		map[string]any{"type": "response.reasoning_summary_text.delta", "output_index": 0, "summary_index": 0, "delta": "Synthetic reasoning: 雪"},
		map[string]any{"type": "response.output_item.done", "output_index": 0, "item": reasoning},
		map[string]any{"type": "response.output_item.added", "output_index": 1, "item": map[string]any{"id": "msg_synthetic", "type": "message", "role": "assistant", "content": []any{}}},
		map[string]any{"type": "response.output_text.delta", "output_index": 1, "content_index": 0, "delta": "Synthetic answer ✓"},
		map[string]any{"type": "response.output_item.done", "output_index": 1, "item": message},
		map[string]any{"type": "response.completed", "response": terminal})
	// CRLF must survive just like native fields (Pi gets a private LF copy).
	wire = bytes.ReplaceAll(wire, []byte("\n"), []byte("\r\n"))
	input := []any{reasoning, message, map[string]any{"role": "user", "content": "synthetic follow-up"}}
	requestBody, err := json.Marshal(map[string]any{"model": "gpt-5.4", "input": input, "store": false, "include": []string{"reasoning.encrypted_content"}, "prompt_cache_key": "synthetic-cache-key"})
	if err != nil {
		t.Fatal(err)
	}
	var wantRequest map[string]any
	if err = json.Unmarshal(requestBody, &wantRequest); err != nil {
		t.Fatal(err)
	}
	// Pi owns only these transport flags. Every other native request field,
	// especially the encrypted reasoning replay item, must remain unchanged.
	wantRequest["stream"] = true
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		raw, readErr := io.ReadAll(r.Body)
		if readErr == nil && r.Header.Get("Content-Encoding") == "zstd" {
			decoder, decodeErr := zstd.NewReader(nil)
			if decodeErr != nil {
				t.Error(decodeErr)
				w.WriteHeader(500)
				return
			}
			raw, readErr = decoder.DecodeAll(raw, nil)
			decoder.Close()
		}
		if readErr != nil {
			t.Error(readErr)
			w.WriteHeader(500)
			return
		}
		var gotRequest map[string]any
		if err := json.Unmarshal(raw, &gotRequest); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if !reflect.DeepEqual(gotRequest, wantRequest) {
			t.Errorf("native reasoning replay request changed:\ngot %s\nwant %#v", raw, wantRequest)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(wire)
	}))
	defer upstream.Close()
	bridge, _ := startPi(t, upstream.URL)
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
			q := contractRequest(upstream.URL)
			q.Payload = requestBody
			actual, err := runContract(t, bridge, q, streaming)
			if err != nil {
				t.Fatal(err)
			}
			if streaming {
				if !bytes.Equal(actual, wire) {
					t.Fatalf("native reasoning/signature/usage SSE changed: %s", actual)
				}
				return
			}
			expected, _ := json.Marshal(terminal)
			var got, want any
			if err := json.Unmarshal(actual, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(expected, &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("full native terminal differs (including signatures and cache details):\ngot %s\nwant %s", actual, expected)
			}
		})
	}
	if attempts.Load() != 2 {
		t.Fatalf("upstream attempts = %d, want 2", attempts.Load())
	}
}

func TestRealPiUpstreamHTTPErrorNoRetryAndSanitized(t *testing.T) {
	for _, status := range []int{401, 429, 503, 307} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var attempts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "0")
				// A local redirect loop must not be followed by Pi or the bridge.
				w.Header().Set("Location", "/must-not-follow")
				w.Header().Set("X-Request-Id", contractCredential().AccessToken)
				w.WriteHeader(status)
				fmt.Fprintf(w, `{"error":{"message":"SYNTHETIC_PRIVATE_UPSTREAM_DETAIL %s"}}`, contractCredential().AccessToken)
			}))
			defer upstream.Close()
			bridge, _ := startPi(t, upstream.URL)
			for _, streaming := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
					before := attempts.Load()
					payload, err := runContract(t, bridge, contractRequest(upstream.URL), streaming)
					requirePiError(t, err, "maybe_sent", status)
					var retry interface{ RetryAfter() *time.Duration }
					if !errors.As(err, &retry) || retry.RetryAfter() == nil || *retry.RetryAfter() != 0 {
						t.Fatalf("Retry-After metadata was not preserved: %v", err)
					}
					if len(payload) != 0 {
						t.Fatalf("HTTP failure exposed upstream body: %s", payload)
					}
					if attempts.Load() != before+1 {
						t.Fatalf("upstream was retried: attempts before=%d after=%d", before, attempts.Load())
					}
				})
			}
		})
	}
}

func TestRealPiRejectsMissingAndIncompleteTerminal(t *testing.T) {
	prefix := fixtureSSE(createdEvent())
	for _, tc := range []struct {
		name string
		tail []byte
	}{
		{"eof", nil},
		{"done_marker_without_terminal", []byte("data: [DONE]\n\n")},
		{"incomplete", fixtureSSE(map[string]any{"type": "response.incomplete", "response": map[string]any{"id": "resp_synthetic", "status": "incomplete", "output": []any{}, "incomplete_details": map[string]any{"reason": "max_output_tokens"}}})},
		{"completed_event_with_incomplete_status", fixtureSSE(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_synthetic", "status": "incomplete", "output": []any{}}})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				w.Write(prefix)
				w.Write(tc.tail)
			}))
			defer upstream.Close()
			bridge, _ := startPi(t, upstream.URL)
			for _, streaming := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
					payload, err := runContract(t, bridge, contractRequest(upstream.URL), streaming)
					requirePiError(t, err, "maybe_sent", 200)
					if !streaming && len(payload) != 0 {
						t.Fatalf("incomplete response exposed as unary result: %s", payload)
					}
					if streaming && !bytes.HasPrefix(payload, prefix) {
						t.Fatalf("valid prefix was lost before terminal failure: %s", payload)
					}
					if bytes.Contains(payload, []byte(`"type":"response.completed"`)) || bytes.Contains(payload, []byte(`"type":"response.incomplete"`)) {
						t.Fatalf("failed terminal forwarded as native success: %s", payload)
					}
				})
			}
			if attempts.Load() != 2 {
				t.Fatalf("terminal failure caused retry: %d attempts", attempts.Load())
			}
		})
	}
}

func TestRealPiUnsupportedBeforeDispatch(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { attempts.Add(1); w.WriteHeader(500) }))
	defer upstream.Close()
	bridge, _ := startPi(t, upstream.URL)
	for _, tc := range []struct {
		name   string
		mutate func(*codex.ExecuteRequest)
	}{
		{"stateful", func(q *codex.ExecuteRequest) {
			q.Payload = json.RawMessage(`{"input":[],"previous_response_id":"resp_synthetic"}`)
		}},
		{"background", func(q *codex.ExecuteRequest) { q.Payload = json.RawMessage(`{"input":[],"background":true}`) }},
		{"stored", func(q *codex.ExecuteRequest) { q.Payload = json.RawMessage(`{"input":[],"store":true}`) }},
		{"format", func(q *codex.ExecuteRequest) { q.Format = "openai" }},
		{"proxy", func(q *codex.ExecuteRequest) { q.ProxyURL = "ftp://127.0.0.1:1" }},
		// Missing input passes Go validation and is rejected by the actual Node
		// validator; this protects not_sent on both sides of the subprocess boundary.
		{"node_missing_input", func(q *codex.ExecuteRequest) { q.Payload = json.RawMessage(`{"model":"gpt-5.4"}`) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, streaming := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
					q := contractRequest(upstream.URL)
					tc.mutate(&q)
					_, err := runContract(t, bridge, q, streaming)
					status := 0
					if tc.name == "node_missing_input" {
						status = 400
					}
					requirePiError(t, err, "not_sent", status)
				})
			}
		})
	}
	if attempts.Load() != 0 {
		t.Fatalf("unsupported request reached upstream %d times", attempts.Load())
	}
}

func TestRealPiClientCancellationReachesUpstream(t *testing.T) {
	for _, mode := range []string{"unary_before_headers", "stream_waiting_for_data", "stream_unconsumed_chunk"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{}, 1)
			cancelled := make(chan struct{}, 1)
			release := make(chan struct{})
			var attempts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				io.Copy(io.Discard, r.Body)
				if mode != "unary_before_headers" {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(200)
					if mode == "stream_unconsumed_chunk" {
						w.Write(fixtureSSE(createdEvent()))
					}
					w.(http.Flusher).Flush()
				}
				select {
				case started <- struct{}{}:
				default:
				}
				select {
				case <-r.Context().Done():
					select {
					case cancelled <- struct{}{}:
					default:
					}
				case <-release:
				}
			}))
			// Release handlers even on assertion failure; never let Server.Close hang.
			defer upstream.Close()
			defer close(release)
			bridge, _ := startPi(t, upstream.URL)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := make(chan error, 1)
			streamReady := make(chan *codex.ExecuteStreamResponse, 1)
			go func() {
				if mode == "unary_before_headers" {
					_, err := bridge.Execute(ctx, "synthetic", contractCredential(), contractRequest(upstream.URL))
					finished <- err
					return
				}
				out, err := bridge.ExecuteStream(ctx, "synthetic", contractCredential(), contractRequest(upstream.URL))
				if err != nil {
					finished <- err
					return
				}
				streamReady <- out
			}()
			select {
			case <-started:
			case <-time.After(10 * time.Second):
				cancel()
				t.Fatal("upstream request never started")
			}
			var stream *codex.ExecuteStreamResponse
			if mode != "unary_before_headers" {
				select {
				case stream = <-streamReady:
				case err := <-finished:
					t.Fatalf("stream open: %v", err)
				case <-time.After(10 * time.Second):
					cancel()
					t.Fatal("stream headers never arrived")
				}
			}
			cancel() // Real Go HTTP client cancellation, not a synthetic bridge frame.
			select {
			case <-cancelled:
			case <-time.After(5 * time.Second):
				t.Fatal("client cancellation did not abort upstream HTTP request")
			}
			if stream == nil {
				select {
				case err := <-finished:
					if err == nil {
						t.Error("cancelled unary succeeded")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("cancelled unary did not finish")
				}
			} else {
				timeout := time.NewTimer(5 * time.Second)
				defer timeout.Stop()
				for {
					select {
					case _, ok := <-stream.Chunks:
						if !ok {
							if attempts.Load() != 1 {
								t.Errorf("cancelled request retried: %d", attempts.Load())
							}
							return
						}
					case <-timeout.C:
						t.Fatal("cancelled stream channel did not close")
					}
				}
			}
			if attempts.Load() != 1 {
				t.Errorf("cancelled request retried: %d", attempts.Load())
			}
		})
	}
}
