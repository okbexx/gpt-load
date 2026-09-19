package cpa

import (
	"context"
	"net/http"

	"gpt-load/internal/subscription/providers/codex"
)

// Local counting is independent of both executor selections and credentials.
// In particular, never delegate this method to base.CountTokensLocal: that
// would silently execute CPA for an explicitly selected Pi route.
func (*piProviderBridge) CountTokensLocal(ctx context.Context, q providerRequest) (providerResponse, error) {
	response, err := codex.EstimatePiTokens(ctx, piCountRequest(q))
	if err != nil {
		return providerResponse{}, err
	}
	headers := make(http.Header)
	headers.Set(localTokenCountHeader, "local-estimate")
	headers.Set("X-GPT-Load-Driver", "pi-experimental")
	return providerResponse{Payload: response.Payload, Headers: headers, Local: true}, nil
}

func (*piProviderBridge) ValidateLocalTokenCount(q providerRequest) error {
	return codex.ValidatePiLocalTokenCount(piCountRequest(q))
}

func piCountRequest(q providerRequest) codex.ExecuteRequest {
	return codex.ExecuteRequest{Model: q.Model, Format: q.Format, Payload: q.Payload}
}

var _ providerLocalTokenCounter = (*piProviderBridge)(nil)
