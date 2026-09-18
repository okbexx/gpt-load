//go:build pi_integration

package cpa

// This suite stops at the Adapter contract (not the HTTP gateway). Only the
// upstream is synthetic: credential storage, preparation, Go bridge, Node and
// the installed, lockfile-pinned pi-ai transport all run for real.
import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"gpt-load/internal/execution"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/protocol"
	"gpt-load/internal/subscription"
)

const piIntegrationSecret = "synthetic-adapter-bridge-secret-not-a-real-key"

func startAdapterPi(t *testing.T, upstream *httptest.Server) string {
	t.Helper()
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "pi-driver/node_modules/@earendil-works/pi-ai/package.json")); err != nil {
		t.Fatal("install pinned pi-driver dependencies with npm ci --ignore-scripts first")
	}
	dir := t.TempDir()
	ca := filepath.Join(dir, "upstream-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"host": "127.0.0.1", "port": 0, "allowedBaseUrls": []string{upstream.URL + "/backend-api"}})
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "bridge.json")
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	stderr, err := os.Create(filepath.Join(dir, "stderr.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stderr.Close() })
	cmd := exec.Command("node", filepath.Join(root, "pi-driver/server.mjs"), "--config", configPath)
	// An explicit environment prevents production credentials/proxies and TLS
	// bypass settings from leaking into the subprocess. TLS remains verified.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "PI_DRIVER_TOKEN=" + piIntegrationSecret, "NODE_EXTRA_CA_CERTS=" + ca}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Node exit: %v", err)
			}
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("Node required forced shutdown")
		}
		if t.Failed() {
			log, _ := os.ReadFile(stderr.Name())
			t.Logf("Node stderr: %s", log)
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
		for s.Scan() {
		}
	}()
	var line string
	select {
	case line = <-lines:
	case <-time.After(20 * time.Second):
		t.Fatal("Node readiness timeout")
	}
	const prefix = "pi-driver listening on "
	if !strings.HasPrefix(line, prefix) {
		t.Fatalf("Node readiness: %q", line)
	}
	endpoint := "http://" + strings.TrimPrefix(line, prefix)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, endpoint+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+piIntegrationSecret)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var health struct {
		Status  string
		Driver  string
		Version int
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || health.Status != "ok" || health.Driver != "pi" || health.Version != 1 {
		t.Fatalf("bridge protocol health: %d %+v", resp.StatusCode, health)
	}
	return endpoint
}

func TestPiIntegrationAdapterChain(t *testing.T) {
	payload, err := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "account-1"}, "exp": 4102444800, "extra": "💡💡💡"})
	if err != nil {
		t.Fatal(err)
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	if !strings.ContainsAny(encodedPayload, "-_") {
		t.Fatal("JWT fixture must exercise Pi's Base64URL parser compatibility")
	}
	token := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + encodedPayload + ".synthetic"
	canonical := credentialJSON(token, "synthetic-refresh-never-used", time.Now().Add(time.Hour))
	fixture, db, _, keys, row := newAdapterFixture(t, canonical)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if strings.Contains(row.Data, token) {
		t.Fatal("fixture credential was not encrypted")
	}
	manager := fixture.credentials.(*subscription.CredentialManager)
	var calls atomic.Int32
	var failing atomic.Bool
	const completed = `{"id":"resp_adapter_fixture","object":"response","status":"completed","model":"gpt-5","output":[],"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":7}},"fixture_extension":{"preserve":true}}`
	wire := []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_adapter_fixture\",\"model\":\"gpt-5\",\"status\":\"in_progress\",\"output\":[]}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + completed + "}\n\n")
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/backend-api/codex/responses" {
			t.Errorf("upstream route: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Chatgpt-Account-Id") != "account-1" {
			t.Error("selected synthetic identity lost across credential/bridge/transport chain")
		}
		if r.Header.Get("Originator") != "pi" {
			t.Errorf("transport originator: %q", r.Header.Get("Originator"))
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
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
		var request map[string]any
		if err := json.Unmarshal(raw, &request); err != nil {
			t.Error(err)
			return
		}
		if request["model"] != "gpt-5" || request["input"] == nil || request["store"] != false {
			t.Errorf("upstream request: %s", raw)
		}
		w.Header().Set("X-Codex-Primary-Used-Percent", fmt.Sprint(12*calls.Load()))
		w.Header().Set("X-Codex-Primary-Window-Minutes", "300")
		w.Header().Set("X-Codex-Primary-Reset-At", "4102444800")
		w.Header().Set("X-Codex-Secondary-Used-Percent", "0.0000001")
		w.Header().Set("X-Codex-Secondary-Window-Minutes", "10080")
		w.Header().Set("X-Request-Id", "synthetic-private-request-id")
		if failing.Load() {
			w.Header().Set("Retry-After", "17")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"code":"rate_limit_exceeded","message":"synthetic-private-error"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(wire)
	}))
	t.Cleanup(upstream.Close)
	endpoint := startAdapterPi(t, upstream)
	adapter, err := NewAdapterWithConfig(manager, fixture.channels, &config.Config{ExperimentalPiEnabled: true, PiBridgeURL: endpoint, PiBridgeSecret: piIntegrationSecret})
	if err != nil {
		t.Fatal(err)
	}
	sentinel := &fakeExecutor{}
	setCodexExecutor(t, adapter, sentinel)
	t.Cleanup(func() {
		sentinel.mu.Lock()
		defer sentinel.mu.Unlock()
		if sentinel.calls != 0 || sentinel.countCalls != 0 {
			t.Errorf("CPA fallback calls: %d count: %d", sentinel.calls, sentinel.countCalls)
		}
	})
	spec := validSpec(t, row, keys)
	spec.TargetConfig, err = json.Marshal(map[string]string{"execution_driver": "pi-experimental", "base_url": upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	spec.Body = []byte(`{"model":"gpt-5","input":[{"role":"user","content":"synthetic fixture"}],"store":false}`)
	checkQuota := func(t *testing.T) {
		t.Helper()
		dirty := manager.DirtyPassiveQuotaObservations(10)
		if len(dirty) != 1 || dirty[0].CredentialID != row.ID || dirty[0].ObservedAtMS <= 0 || len(dirty[0].Windows) != 2 {
			t.Fatalf("manager quota observation: %+v", dirty)
		}
		want := map[string]float64{"primary": float64(12 * calls.Load()), "secondary": 0.0000001}
		for _, window := range dirty[0].Windows {
			expected, ok := want[window.ID]
			if !ok || window.SourceID != "codex" || window.Used == nil || *window.Used != expected {
				t.Errorf("quota window: %+v", window)
			}
			if window.ID == "primary" && (window.WindowSeconds == nil || *window.WindowSeconds != 18000 || window.ResetAtMS == nil || *window.ResetAtMS != 4102444800000) {
				t.Errorf("primary quota timing: %+v", window)
			}
		}
	}
	t.Run("unary_cached_usage_and_quota", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		before := calls.Load()
		got := adapter.Execute(ctx, spec)
		if got.Error != nil {
			t.Fatalf("execute: %+v", got.Error)
		}
		if err := got.Validate(); err != nil {
			t.Error(err)
		}
		if got.Header.Get("X-GPT-Load-Driver") != "pi-experimental" || got.Header.Get("Content-Type") != "application/json" || got.UpstreamRequestID != "" {
			t.Errorf("response metadata: %+v", got)
		}
		if got.Usage == nil || got.Usage.Normalized.Tokens.CacheRead != 7 || got.Usage.Normalized.Tokens.UncachedInput != 3 || got.Usage.Normalized.Tokens.Output != 4 {
			t.Errorf("cached usage lost: %+v", got.Usage)
		}
		if !bytes.Contains(got.Body, []byte(`"fixture_extension"`)) {
			t.Errorf("native response lost extension: %s", got.Body)
		}
		if calls.Load()-before != 1 {
			t.Errorf("upstream attempts: %d", calls.Load()-before)
		}
		checkQuota(t)
	})
	t.Run("stream_completed_usage_and_quota", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		before := calls.Load()
		var data bytes.Buffer
		ready := 0
		got := adapter.ExecuteStream(ctx, spec, func(event execution.StreamEvent) error {
			if event.Kind == execution.StreamEventReady {
				ready++
				if event.Header.Get("X-GPT-Load-Driver") != "pi-experimental" {
					t.Error("stream driver header lost")
				}
			}
			if event.Kind == execution.StreamEventData {
				data.Write(event.Data)
			}
			return nil
		})
		if got.Error != nil {
			t.Fatalf("stream: %+v", got.Error)
		}
		if err := got.Validate(); err != nil {
			t.Error(err)
		}
		if ready != 1 || calls.Load()-before != 1 {
			t.Errorf("ready=%d upstream=%d", ready, calls.Load()-before)
		}
		// Native streaming usage belongs to response.completed, not StreamResult.
		found := false
		for _, line := range strings.Split(data.String(), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var event struct {
				Type     string          `json:"type"`
				Response json.RawMessage `json:"response"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
				t.Error(err)
				continue
			}
			if event.Type == "response.completed" {
				found = true
				usage := responseUsage(spec, event.Response)
				if usage == nil || usage.Normalized.Tokens.CacheRead != 7 || usage.Normalized.Tokens.UncachedInput != 3 || usage.Normalized.Tokens.Output != 4 {
					t.Errorf("completed usage lost: %+v", usage)
				}
			}
		}
		if !found {
			t.Errorf("missing completion: %s", data.Bytes())
		}
		checkQuota(t)
	})
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("upstream_failure_stream_%t", stream), func(t *testing.T) {
			failing.Store(true)
			defer failing.Store(false)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			before := calls.Load()
			var evidence *execution.ErrorEvidence
			var dispatch execution.DispatchState
			if stream {
				got := adapter.ExecuteStream(ctx, spec, func(execution.StreamEvent) error { t.Error("failure emitted stream data"); return nil })
				evidence = got.Error
				dispatch = got.DispatchState
			} else {
				got := adapter.Execute(ctx, spec)
				evidence = got.Error
				dispatch = got.DispatchState
			}
			if evidence == nil || evidence.StatusCode != 429 || evidence.Type != "pi_driver_error" || evidence.Code != "pi_upstream_http_error" || evidence.Hint != execution.FailureHintRateLimited || evidence.ScopeHint != "" || evidence.RetryAfter != 17*time.Second || evidence.ReplaySafety != execution.ReplaySafetyUnknown || dispatch != execution.DispatchMaybeSent {
				t.Errorf("cross-layer error/retry evidence: %+v dispatch=%s", evidence, dispatch)
			}
			if evidence != nil && strings.Contains(evidence.Summary, "synthetic-private") {
				t.Error("upstream private failure leaked")
			}
			if calls.Load()-before != 1 {
				t.Errorf("upstream retry/fallback: %d", calls.Load()-before)
			}
		})
	}
	t.Run("default_driver_unchanged", func(t *testing.T) {
		defaultCPA := &fakeExecutor{}
		setCodexExecutor(t, adapter, defaultCPA)
		defer setCodexExecutor(t, adapter, sentinel)
		defaultSpec := spec
		defaultSpec.TargetConfig = json.RawMessage(`{}`)
		before := calls.Load()
		adapter.Execute(t.Context(), defaultSpec)
		defaultCPA.mu.Lock()
		defer defaultCPA.mu.Unlock()
		if defaultCPA.calls != 1 || calls.Load() != before {
			t.Errorf("default driver changed: CPA=%d upstream=%d", defaultCPA.calls, calls.Load()-before)
		}
	})
	t.Run("config_opt_in_required_before_prepare", func(t *testing.T) {
		disabled, err := NewAdapterWithConfig(manager, fixture.channels, &config.Config{PiBridgeURL: endpoint, PiBridgeSecret: piIntegrationSecret})
		if err != nil {
			t.Fatal(err)
		}
		setCodexExecutor(t, disabled, sentinel)
		preparer := &fakeCredentialPreparer{delegate: manager}
		disabled.credentials = preparer
		before := calls.Load()
		got := disabled.Execute(t.Context(), spec)
		if got.Error == nil || got.Error.Code != "pi_experimental_disabled" || got.DispatchState != execution.DispatchNotSent || preparer.calls != 0 || calls.Load() != before {
			t.Errorf("process opt-in bypass: %+v, prepares=%d", got, preparer.calls)
		}
	})
	t.Run("unsupported_before_prepare", func(t *testing.T) {
		preparer := &fakeCredentialPreparer{delegate: manager}
		adapter.credentials = preparer
		defer func() { adapter.credentials = manager }()
		unsupported := spec
		unsupported.ClientProtocol = protocol.OpenAICompletions
		before := calls.Load()
		unary := adapter.Execute(t.Context(), unsupported)
		stream := adapter.ExecuteStream(t.Context(), unsupported, func(execution.StreamEvent) error { t.Error("unsupported emitted event"); return nil })
		for _, e := range []*execution.ErrorEvidence{unary.Error, stream.Error} {
			if e == nil || e.Code != "pi_unsupported_capability" {
				t.Errorf("unsupported evidence: %+v", e)
			}
		}
		if unary.DispatchState != execution.DispatchNotSent || stream.DispatchState != execution.DispatchNotSent || preparer.calls != 0 || calls.Load() != before {
			t.Error("unsupported request reached preparation or upstream")
		}
	})
}
