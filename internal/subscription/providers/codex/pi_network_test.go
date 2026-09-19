package codex

import "testing"

func TestPiExplicitProxyAdmission(t *testing.T) {
	for _, proxy := range []string{"", "direct", "http://127.0.0.1:8080", "https://proxy.invalid:443", "http://user:pass@127.0.0.1:8080/", "http://[::1]:8080"} {
		q := ExecuteRequest{Model: "gpt-5", Format: "openai-response", Payload: []byte(`{"input":"synthetic"}`), ProxyURL: proxy}
		if err := ValidatePiRequest(q); err != nil {
			t.Errorf("valid explicit proxy rejected: %q: %v", proxy, err)
		}
	}
	for _, proxy := range []string{"ftp://proxy.invalid", "http://proxy.invalid/path", "http://proxy.invalid?", "http://proxy.invalid#", "http://proxy.invalid:0", "http://proxy.invalid:65536", "http://user:%0Asecret@proxy.invalid", "http://proxy.invalid\\escape"} {
		q := ExecuteRequest{Model: "gpt-5", Format: "openai-response", Payload: []byte(`{"input":"synthetic"}`), ProxyURL: proxy}
		if err := ValidatePiRequest(q); err == nil {
			t.Errorf("invalid proxy admitted: %q", proxy)
		}
	}
	if err := ValidatePiRequest(ExecuteRequest{Format: "openai-response", Payload: []byte(`{"input":"synthetic"}`), ProxyFromEnvironment: true}); err == nil {
		t.Fatal("sidecar must not choose proxy from its own environment")
	}
}
