package cpa

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"gpt-load/internal/channel"
	"gpt-load/internal/execution"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/protocol"
	"gpt-load/internal/subscription"
	"gpt-load/internal/subscription/providers/codex"
	"strings"
)

// NewAdapterWithConfig enables a separate Pi client only with process opt-in.
// It does not replace the default executor or register Pi as a parity provider.
func NewAdapterWithConfig(credentials *subscription.CredentialManager, channels *channel.Registry, cfg *config.Config) (*Adapter, error) {
	a := NewAdapter(credentials, channels)
	if cfg == nil || !cfg.ExperimentalPiEnabled {
		return a, nil
	}
	if cfg.PiBridgeSecret == cfg.AuthKey || cfg.PiBridgeSecret == cfg.EncryptionKey {
		return nil, fmt.Errorf("PI_BRIDGE_SECRET must be separate from application keys")
	}
	pi, err := codex.NewPiExecutor(cfg.PiBridgeURL, cfg.PiBridgeSecret)
	if err != nil {
		return nil, err
	}
	a.piExecutor = pi
	return a, nil
}

// Deliberately does not embed codexProviderBridge: doing so would accidentally
// inherit CPA local counting and websocket execution as implicit fallbacks.
type piProviderBridge struct{ base *codexProviderBridge }

func (p *piProviderBridge) ProviderKind() channel.ProviderKind  { return channel.ProviderCodex }
func (p *piProviderBridge) UpstreamProtocol() protocol.Protocol { return protocol.OpenAIResponses }
func (p *piProviderBridge) ValidateRouteCapability(r channel.RouteDescriptor) error {
	if r.ClientProtocol != protocol.OpenAIResponses || r.Operation != execution.OperationResponsesCreate || r.RouteMode != execution.RouteNative {
		return fmt.Errorf("experimental pi only supports native HTTP Responses")
	}
	return nil
}
func (p *piProviderBridge) ParseCredential(raw []byte) (providerCredential, error) {
	return p.base.ParseCredential(raw)
}
func (p *piProviderBridge) Execute(ctx context.Context, id string, c providerCredential, q providerRequest) (providerResponse, error) {
	return p.base.Execute(ctx, id, c, piProviderRequest(q))
}
func (p *piProviderBridge) ExecuteStream(ctx context.Context, id string, c providerCredential, q providerRequest) (*providerStreamResponse, error) {
	return p.base.ExecuteStream(ctx, id, c, piProviderRequest(q))
}
func (p *piProviderBridge) ClassifyError(ctx context.Context, err error, c providerCredential) (int, *execution.ErrorEvidence) {
	return p.base.ClassifyError(ctx, err, c)
}

func piProviderRequest(q providerRequest) providerRequest {
	// Group base_url is an API root, whereas Pi expects the backend-api base.
	if q.BaseURL != "" {
		q.BaseURL = strings.TrimRight(q.BaseURL, "/") + "/backend-api"
	}
	q.ContinuityKey = scopedPiContinuityKey(q.ContinuityKey, q.CredentialID, q.CredentialIdentityGeneration, q.Model)
	return q
}

func scopedPiContinuityKey(key string, credentialID uint, generation uint64, model string) string {
	if key == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("gpt-load-pi/v1\x00%s\x00%d\x00%d\x00%s", key, credentialID, generation, model)))
	return "pi-" + hex.EncodeToString(digest[:16])
}
func (p *piProviderBridge) ValidateRequest(q providerRequest) error {
	return codex.ValidatePiRequest(codex.ExecuteRequest{Model: q.Model, Payload: q.Payload, Format: q.Format, RequestPath: q.RequestPath, Headers: q.Headers, ConfiguredHeaders: q.ConfiguredHeaders, ProxyURL: q.ProxyURL, ProxyFromEnvironment: q.ProxyFromEnvironment})
}

func (a *Adapter) piAdmission(spec execution.AttemptSpec, websocket bool) string {
	if channel.ID(spec.ChannelID) != channel.Codex {
		return ""
	}
	var selection struct {
		Driver string `json:"execution_driver"`
	}
	if json.Unmarshal(spec.TargetConfig, &selection) != nil || selection.Driver != "pi-experimental" {
		return ""
	}
	if websocket || spec.ClientProtocol != protocol.OpenAIResponses || spec.Operation != execution.OperationResponsesCreate || spec.RouteMode != execution.RouteNative {
		return "pi_unsupported_capability"
	}
	if a == nil || a.piExecutor == nil {
		return "pi_experimental_disabled"
	}
	return ""
}

var _ providerBridge = (*piProviderBridge)(nil)
