//go:build pi_integration

package cpa

// This is an Adapter-boundary integration test, not an HTTP gateway test.
// Credential preparation, bridge, Pi transport, CONNECT and verified TLS are real.
import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"gpt-load/internal/execution"
	"gpt-load/internal/outboundproxy"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/protocol"
	"gpt-load/internal/subscription"
)

// Only the named local origin can be tunneled. Track hijacked connections because
// httptest.Server.Close does not own them; close both ends and join both pumps.
func startPiAuthenticatedTunnel(t *testing.T, target, auth string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	var mu sync.Mutex
	var workers sync.WaitGroup
	conns := make(map[net.Conn]bool)
	closing := false
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodConnect || r.Host != target || r.Header.Get("Proxy-Authorization") != auth {
			t.Error("CONNECT route or proxy authentication mismatch")
			http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
			return
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("Chatgpt-Account-Id") != "" {
			t.Error("origin credentials leaked onto CONNECT")
		}
		mu.Lock()
		if closing {
			mu.Unlock()
			http.Error(w, "closed", 503)
			return
		}
		workers.Add(1)
		mu.Unlock()
		defer workers.Done()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		origin, err := (&net.Dialer{}).DialContext(ctx, "tcp", target)
		if err != nil {
			t.Error(err)
			http.Error(w, "dial failed", 502)
			return
		}
		defer origin.Close()
		client, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer client.Close()
		mu.Lock()
		if closing {
			mu.Unlock()
			return
		}
		conns[client], conns[origin] = true, true
		mu.Unlock()
		defer func() { mu.Lock(); delete(conns, client); delete(conns, origin); mu.Unlock() }()
		_ = client.SetDeadline(time.Now().Add(15 * time.Second))
		_ = origin.SetDeadline(time.Now().Add(15 * time.Second))
		if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			t.Error(err)
			return
		}
		if err := buffered.Flush(); err != nil {
			t.Error(err)
			return
		}
		done := make(chan struct{})
		go func() { defer close(done); _, _ = io.Copy(origin, buffered); _ = origin.Close() }()
		_, _ = io.Copy(client, origin)
		_ = client.Close()
		_ = origin.Close()
		<-done
	}))
	t.Cleanup(func() {
		mu.Lock()
		closing = true
		for conn := range conns {
			_ = conn.Close()
		}
		mu.Unlock()
		proxy.Close()
		done := make(chan struct{})
		go func() { workers.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("CONNECT pumps failed to stop")
		}
	})
	return proxy, &calls
}

func TestPiIntegrationFrozenProxyPolicy(t *testing.T) {
	token := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"account-1"},"exp":4102444800}`)) + ".synthetic"
	fixture, db, _, keys, row := newAdapterFixture(t, credentialJSON(token, "synthetic-refresh-unused", time.Now().Add(time.Hour)))
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if strings.Contains(row.Data, token) {
		t.Fatal("credential fixture is not encrypted")
	}
	const proxyUser = "synthetic-proxy-user"
	const proxyPassword = "synthetic-proxy:p@ss"
	proxyAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(proxyUser+":"+proxyPassword))
	const text = "proxy policy verified"
	const completed = `{"id":"resp_proxy","object":"response","status":"completed","model":"gpt-5","output":[{"id":"msg_proxy","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"proxy policy verified","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`
	wire := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_proxy\",\"model\":\"gpt-5\",\"status\":\"in_progress\",\"output\":[]}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + completed + "}\n\n"
	var originCalls atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originCalls.Add(1)
		if r.TLS == nil || r.Method != http.MethodPost || r.URL.Path != "/backend-api/codex/responses" {
			t.Error("wrong TLS origin route")
		}
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Chatgpt-Account-Id") != "account-1" {
			t.Error("original credential authorization was replaced")
		}
		if r.Header.Get("Originator") != "pi" {
			t.Errorf("originator=%q", r.Header.Get("Originator"))
		}
		if r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("Proxy-Connection") != "" {
			t.Error("proxy headers leaked to TLS origin")
		}
		for _, values := range r.Header {
			for _, value := range values {
				if strings.Contains(value, proxyUser) || strings.Contains(value, proxyPassword) || strings.Contains(value, proxyAuth) || strings.Contains(value, piIntegrationSecret) {
					t.Error("proxy/bridge credential leaked to origin")
				}
			}
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, wire)
	}))
	t.Cleanup(upstream.Close)
	target := strings.TrimPrefix(upstream.URL, "https://")
	proxy, connects := startPiAuthenticatedTunnel(t, target, proxyAuth)
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxyURL.User = url.UserPassword(proxyUser, proxyPassword)
	// The child has a scrubbed environment. Only the frozen AttemptSpec can select
	// this proxy; Pi cannot rediscover it through ambient process configuration.
	endpoint := startAdapterPi(t, upstream)
	adapter, err := NewAdapterWithConfig(fixture.credentials.(*subscription.CredentialManager), fixture.channels, &config.Config{ExperimentalPiEnabled: true, PiBridgeURL: endpoint, PiBridgeSecret: piIntegrationSecret})
	if err != nil {
		t.Fatal(err)
	}
	sentinel := &fakeExecutor{err: errors.New("CPA fallback forbidden")}
	setCodexExecutor(t, adapter, sentinel)
	assertNoFallback := func(t *testing.T) {
		t.Helper()
		sentinel.mu.Lock()
		defer sentinel.mu.Unlock()
		if sentinel.calls != 0 || sentinel.countCalls != 0 {
			t.Errorf("CPA fallback calls=%d countCalls=%d", sentinel.calls, sentinel.countCalls)
		}
	}
	t.Cleanup(func() { assertNoFallback(t) })
	base := validSpec(t, row, keys)
	base.TargetConfig, err = json.Marshal(map[string]string{"execution_driver": "pi-experimental", "base_url": upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	base.Proxy = outboundproxy.Effective{Config: outboundproxy.Config{Mode: outboundproxy.ModeCustom, URL: proxyURL.String()}, Source: outboundproxy.SourceGroup}

	// A closed local listener is a refused proxy, not a synthetic bridge failure.
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadURL := "http://" + dead.Addr().String()
	if err := dead.Close(); err != nil {
		t.Fatal(err)
	}
	for _, choice := range []string{"custom", "direct", "dead"} {
		for _, converted := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/converted=%t/stream=%t", choice, converted, stream), func(t *testing.T) {
					spec := base
					if choice == "direct" {
						spec.Proxy.Config = outboundproxy.Config{Mode: outboundproxy.ModeDirect}
					}
					if choice == "dead" {
						spec.Proxy.Config.URL = deadURL
					}
					if converted {
						spec.RouteMode, spec.ClientProtocol, spec.Operation = execution.RouteConverted, protocol.OpenAICompletions, execution.OperationChatCompletion
						spec.Path = "/v1/chat/completions"
						spec.Body = []byte(fmt.Sprintf(`{"model":"gpt-5","messages":[{"role":"user","content":"fixture"}],"stream":%t}`, stream))
					} else {
						spec.Body = []byte(fmt.Sprintf(`{"model":"gpt-5","input":"fixture","stream":%t}`, stream))
					}
					beforeOrigin, beforeConnect := originCalls.Load(), connects.Load()
					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
					defer cancel()
					var evidence *execution.ErrorEvidence
					var body bytes.Buffer
					ready := 0
					if stream {
						got := adapter.ExecuteStream(ctx, spec, func(event execution.StreamEvent) error {
							if event.Kind == execution.StreamEventReady {
								ready++
								if event.Header.Get("X-GPT-Load-Driver") != "pi-experimental" {
									t.Error("missing Pi stream marker")
								}
							}
							if event.Kind == execution.StreamEventData {
								body.Write(event.Data)
							}
							return nil
						})
						evidence = got.Error
						if evidence == nil {
							if err := got.Validate(); err != nil {
								t.Error(err)
							}
						}
					} else {
						got := adapter.Execute(ctx, spec)
						evidence = got.Error
						body.Write(got.Body)
						if evidence == nil {
							if err := got.Validate(); err != nil {
								t.Error(err)
							}
							if got.Header.Get("X-GPT-Load-Driver") != "pi-experimental" {
								t.Error("missing Pi unary marker")
							}
						}
					}
					assertNoFallback(t)
					if choice == "dead" {
						if ctx.Err() != nil {
							t.Errorf("dead proxy failed only at request deadline: %v", ctx.Err())
						}
						if evidence == nil || evidence.Type != "pi_driver_error" {
							t.Errorf("dead proxy did not fail through Pi: %+v", evidence)
						}
						if originCalls.Load() != beforeOrigin || connects.Load() != beforeConnect {
							t.Error("dead proxy fell back to a reachable transport")
						}
						if ready != 0 || body.Len() != 0 {
							t.Error("dead proxy emitted successful response data")
						}
						return
					}
					if evidence != nil {
						t.Fatalf("Pi execution: %+v", evidence)
					}
					if originCalls.Load()-beforeOrigin != 1 {
						t.Errorf("origin attempts=%d", originCalls.Load()-beforeOrigin)
					}
					wantConnect := int32(0)
					if choice == "custom" {
						wantConnect = 1
					}
					if connects.Load()-beforeConnect != wantConnect {
						t.Errorf("CONNECT attempts=%d want=%d", connects.Load()-beforeConnect, wantConnect)
					}
					if stream {
						if ready != 1 || body.Len() == 0 {
							t.Errorf("stream ready=%d bytes=%d", ready, body.Len())
						}
						if converted && !bytes.Contains(body.Bytes(), []byte("[DONE]")) {
							t.Error("converted stream missing terminator")
						}
						if !converted && !bytes.Contains(body.Bytes(), []byte("response.completed")) {
							t.Error("native stream missing completion")
						}
					} else {
						path := "output.0.content.0.text"
						if converted {
							path = "choices.0.message.content"
						}
						if gjson.GetBytes(body.Bytes(), path).String() != text {
							t.Errorf("response content: %s", body.Bytes())
						}
					}
				})
			}
		}
	}
}
