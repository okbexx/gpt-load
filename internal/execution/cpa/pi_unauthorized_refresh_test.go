package cpa

import (
	"net/http"
	"testing"
	"time"

	"gpt-load/internal/execution"
	"gpt-load/internal/health"
)

func TestPiUnauthorizedHealthRefreshRequiresRejectedBeforeProcessing(t *testing.T) {
	evidence := &execution.ErrorEvidence{
		Kind:         execution.ErrorKindHTTP,
		StatusCode:   http.StatusUnauthorized,
		OriginHint:   execution.ErrorOriginUpstream,
		Hint:         execution.FailureHintRefreshRequired,
		ReplaySafety: execution.ReplaySafetyRejectedBeforeProcessing,
	}
	decision := health.JudgeExecution(health.ExecutionAttempt{
		DispatchState: execution.DispatchMaybeSent,
		StatusCode:    http.StatusUnauthorized,
		Evidence:      evidence,
		Now:           time.Unix(100, 0),
	}, health.DecisionContext{
		CredentialRefreshable: true,
		Method:                http.MethodPost,
		Operation:             execution.OperationResponsesCreate,
	})
	if decision.Retry != health.RetryRefreshCredential || decision.Effect != health.EffectNone ||
		decision.Scope != execution.ErrorScopeCredential || decision.RuleID != "auth.refresh_required" {
		t.Fatalf("maybe_sent Pi 401 decision = %#v", decision)
	}
	if err := decision.Validate(); err != nil {
		t.Fatalf("invalid refresh decision: %v", err)
	}
}

func TestPiUnauthorizedNotSentRemainsInternalAndDoesNotRefresh(t *testing.T) {
	decision := health.JudgeExecution(health.ExecutionAttempt{
		DispatchState: execution.DispatchNotSent,
		StatusCode:    http.StatusUnauthorized,
		Evidence: &execution.ErrorEvidence{
			Kind:       execution.ErrorKindInternal,
			OriginHint: execution.ErrorOriginInternal,
			StatusCode: 0,
			Code:       "pi_unauthorized",
		},
	}, health.DecisionContext{
		CredentialRefreshable: true,
		Method:                http.MethodPost,
		Operation:             execution.OperationResponsesCreate,
	})
	if decision.Retry != health.RetryNone || decision.Effect != health.EffectNone ||
		decision.Origin != execution.ErrorOriginInternal || decision.Scope != "" {
		t.Fatalf("not_sent Pi 401 decision = %#v", decision)
	}
	if err := decision.Validate(); err != nil {
		t.Fatalf("invalid local decision: %v", err)
	}
}
