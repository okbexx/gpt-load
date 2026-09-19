package cpa

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"

	"gpt-load/internal/channel"
	"gpt-load/internal/execution"
	"gpt-load/internal/protocol"
)

// net/http caches proxy environment process-wide. Each case is a clean child;
// this test never sends network traffic or inherits production environment keys.
func TestPiEnvironmentProxyFrozenInGateway(t *testing.T) {
	if os.Getenv("PI_PROXY_TEST_CHILD") == "1" {
		spec := execution.AttemptSpec{ChannelID: string(channel.Codex), ClientProtocol: protocol.OpenAIResponses, Operation: execution.OperationResponsesCreate,
			ClientModel: "gpt-5", UpstreamModel: "gpt-5", Body: []byte(`{"input":"fixture"}`), TargetConfig: json.RawMessage(`{"execution_driver":"pi-experimental"}`)}
		q, err := bridgeRequest(spec, cpaProxySettings{FromEnvironment: true}, "https://upstream.example.invalid", false)
		if err != nil || q.ProxyFromEnvironment || q.ProxyURL != os.Getenv("PI_PROXY_TEST_WANT") {
			t.Fatalf("proxy not frozen correctly: URL=%q environment=%v err=%v", q.ProxyURL, q.ProxyFromEnvironment, err)
		}
		spec.TargetConfig = json.RawMessage(`{}`)
		q, err = bridgeRequest(spec, cpaProxySettings{FromEnvironment: true}, "https://upstream.example.invalid", false)
		if err != nil || !q.ProxyFromEnvironment || q.ProxyURL != "" {
			t.Fatalf("default CPA proxy behavior changed: %+v %v", q, err)
		}
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, proxy, noProxy, want string }{
		{"configured", "http://127.0.0.1:3128", "", "http://127.0.0.1:3128"},
		{"no-proxy", "http://127.0.0.1:3128", "upstream.example.invalid", "direct"},
		{"empty-environment", "", "", "direct"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(executable, "-test.run=^TestPiEnvironmentProxyFrozenInGateway$", "-test.count=1")
			cmd.Env = []string{"PI_PROXY_TEST_CHILD=1", "HTTPS_PROXY=" + tc.proxy, "NO_PROXY=" + tc.noProxy, "PI_PROXY_TEST_WANT=" + tc.want}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("child: %v\n%s", err, out)
			}
		})
	}
}
