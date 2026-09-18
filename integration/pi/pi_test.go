//go:build pi_integration

package piintegration_test

// These tests use the actual Go bridge and pinned pi-ai package. The upstream
// and credentials are explicitly synthetic fixtures, never real OpenAI accounts.
import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"gpt-load/internal/subscription/providers/codex"
)

func fixtureToken(account string) string {
	p, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": account}, "exp": 4102444800})
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + base64.RawURLEncoding.EncodeToString(p) + ".fixture"
}

func startPi(t *testing.T, upstream string) (codex.Executor, string) {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(root, "pi-driver/node_modules/@earendil-works/pi-ai/package.json")); err != nil {
		t.Fatal("run npm ci --ignore-scripts in pi-driver before tagged integration test")
	}
	token := "synthetic-local-bridge-secret-for-integration"
	dir := t.TempDir()
	config, _ := json.Marshal(map[string]any{"host": "127.0.0.1", "port": 0, "allowedBaseUrls": []string{upstream}})
	configPath := filepath.Join(dir, "bridge.json")
	if err = os.WriteFile(configPath, config, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "node", filepath.Join(root, "pi-driver/server.mjs"), "--config", configPath)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "PI_DRIVER_TOKEN=" + token}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Pi child did not stop")
		}
	})
	lines := make(chan string, 1)
	go func() {
		s := bufio.NewScanner(stdout)
		if s.Scan() {
			lines <- s.Text()
		} else {
			lines <- ""
		}
	}()
	var line string
	select {
	case line = <-lines:
	case <-time.After(20 * time.Second):
		t.Fatal("Pi bridge readiness timeout")
	}
	const prefix = "pi-driver listening on "
	if !strings.HasPrefix(line, prefix) {
		t.Fatalf("Pi did not announce readiness: %q", line)
	}
	endpoint := "http://" + strings.TrimPrefix(line, prefix)
	client := &http.Client{Timeout: 5 * time.Second}
	healthRequest, err := http.NewRequest(http.MethodGet, endpoint+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	healthRequest.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(healthRequest)
	if err != nil {
		t.Fatal(err)
	}
	health, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Contains(health, []byte(`"pi"`)) {
		t.Fatalf("health response %d %s", resp.StatusCode, health)
	}
	bridge, err := codex.NewPiExecutor(endpoint, token)
	if err != nil {
		t.Fatal(err)
	}
	return bridge, endpoint
}

func TestRealPiBridgeNativeWireRoundTrip(t *testing.T) {
	var mu sync.Mutex
	var seen []map[string]any
	var identities []string
	terminal := map[string]any{"id": "resp_fixture", "object": "response", "status": "completed", "model": "gpt-5.4", "output": []any{
		map[string]any{"type": "function_call", "id": "fc_one", "call_id": "call_one", "name": "one", "arguments": "{}", "status": "completed"},
		map[string]any{"type": "function_call", "id": "fc_two", "call_id": "call_two", "name": "two", "arguments": "{\"x\":1}", "status": "completed"},
	}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 4, "total_tokens": 14, "input_tokens_details": map[string]any{"cached_tokens": 7}}, "fixture_extension": map[string]any{"preserve": true}}
	events := []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": "resp_fixture", "model": "gpt-5.4", "status": "in_progress", "output": []any{}}},
		{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "function_call", "id": "fc_one", "call_id": "call_one", "name": "one", "arguments": ""}},
		{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": "fc_one", "delta": "{}"},
		{"type": "response.output_item.added", "output_index": 1, "item": map[string]any{"type": "function_call", "id": "fc_two", "call_id": "call_two", "name": "two", "arguments": ""}},
		{"type": "response.function_call_arguments.delta", "output_index": 1, "item_id": "fc_two", "delta": "{\"x\":1}"},
		{"type": "fixture.unknown_event", "opaque": "must-survive"},
		{"type": "response.completed", "response": terminal},
	}
	var wire bytes.Buffer
	for _, event := range events {
		b, _ := json.Marshal(event)
		fmt.Fprintf(&wire, "event: %s\ndata: %s\n\n", event["type"], b)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Error(readErr)
		}
		if r.Header.Get("Content-Encoding") == "zstd" {
			decoder, err := zstd.NewReader(nil)
			if err != nil {
				t.Fatal(err)
			}
			raw, err = decoder.DecodeAll(raw, nil)
			decoder.Close()
			if err != nil {
				t.Error(err)
			}
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		seen = append(seen, body)
		identities = append(identities, r.Header.Get("chatgpt-account-id"))
		mu.Unlock()
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Error("upstream missing bearer credential")
		}
		if r.Header.Get("Originator") != "pi" {
			t.Errorf("not Pi transport: originator %q", r.Header.Get("Originator"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "fixture-request")
		w.Header().Set("X-Codex-Primary-Used-Percent", "12")
		w.Write(wire.Bytes())
	}))
	defer upstream.Close()
	bridge, _ := startPi(t, upstream.URL)
	body := json.RawMessage(`{"model":"gpt-5.4","instructions":"synthetic fixture","input":[{"role":"user","content":"fixture"}],"store":false,"tools":[{"type":"function","name":"one","parameters":{"type":"object"}},{"type":"function","name":"two","parameters":{"type":"object"}}],"parallel_tool_calls":true}`)
	q := codex.ExecuteRequest{Model: "gpt-5.4", Format: "openai-response", RequestPath: "/v1/responses", Payload: body, BaseURL: upstream.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	credential := codex.Credential{AccessToken: fixtureToken("acct-fixture-a"), AccountID: "acct-fixture-a"}
	unary, err := bridge.Execute(ctx, "fixture-a", credential, q)
	if err != nil {
		t.Fatal(err)
	}
	var actual map[string]any
	if err = json.Unmarshal(unary.Payload, &actual); err != nil {
		t.Fatal(err)
	}
	if actual["fixture_extension"] == nil || len(actual["output"].([]any)) != 2 {
		t.Fatalf("unary native response lost fields: %s", unary.Payload)
	}
	credential = codex.Credential{AccessToken: fixtureToken("acct-fixture-b"), AccountID: "acct-fixture-b"}
	stream, err := bridge.ExecuteStream(ctx, "fixture-b", credential, q)
	if err != nil {
		t.Fatal(err)
	}
	var received bytes.Buffer
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		received.Write(chunk.Payload)
	}
	if !bytes.Equal(received.Bytes(), wire.Bytes()) {
		t.Fatalf("native SSE changed:\nactual=%s\nexpected=%s", received.Bytes(), wire.Bytes())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("expected exactly two upstream attempts, got %d", len(seen))
	}
	if strings.Join(identities, ",") != "acct-fixture-a,acct-fixture-b" {
		t.Fatalf("credential isolation failed: %v", identities)
	}
	for _, request := range seen {
		if request["instructions"] != "synthetic fixture" || request["parallel_tool_calls"] != true || len(request["tools"].([]any)) != 2 {
			t.Fatalf("request semantics lost: %#v", request)
		}
	}
}
