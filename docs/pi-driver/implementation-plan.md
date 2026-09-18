# Optional Pi execution driver Implementation Plan

> **For Hermes:** Use subagent-driven-development skill to implement this plan task-by-task.

**Goal:** Add an explicitly selectable pi-ai execution backend without removing CPA/Bifrost or losing any existing GPT-Load capability; full Pi parity is a release gate, not a marketing claim.

**Architecture:** GPT-Load remains sole owner of credentials, refresh persistence, scheduling, affinity, health, quotas and retries. A local Node service hosts pinned pi-ai, performs a single explicitly selected execution and returns lossless native events and transport evidence through a versioned bridge. No Agent loop, tool execution, hidden fallback or automatic account selection is allowed in the Node service.

**Tech Stack:** Existing Go/Gin/Vue application; pi-ai 0.85.1; Node >=22.19; Go 1.27.1 for local verification.

---

## Baseline and scope

- Upstream: tbphp/gpt-load, commit b517f713204ea6507a0451e7988e08e428ca2bca.
- Fork: okbexx/gpt-load, branch feat/pi-driver.
- All existing API channels and Codex/Claude/Antigravity/Grok subscription capabilities must remain available through their original drivers.
- Every pre-existing subscription capability must have a unique inventory ID, baseline source evidence, Pi status, and verification evidence before declaring full parity.
- Initial native Responses vertical slice is an implementation milestone, NOT fulfillment of full parity. Unsupported operations must be rejected before dispatch, never silently stripped or sent through CPA.
- Do not use production credentials or perform real paid requests without separate authorization. Tests must use explicitly identified synthetic fixtures and local upstream servers; never represent fixtures as live results.

## Task 1: Baseline capability inventory

Create `docs/pi-driver/capabilities.json` and `docs/pi-driver/capabilities.md` from channel declarations, provider implementations and tests. Include all four subscription channels and platform cross-cutting contracts. Verify IDs and record counts programmatically. Use supported/unsupported distinctions from actual baseline, not expected features. Unknown Pi parity is unverified.

## Task 2: Real pi-ai bridge vertical slice

Create isolated `pi-driver/` npm package with pinned dependency and lockfile. Follow red/green contract tests. Implement loopback-only authenticated HTTP process and a single-attempt Codex native Responses execution using the real pi-ai library. Preserve complete original request through onPayload when semantically valid; preserve native upstream event JSON instead of reconstructing it from lossy normalized deltas. Report native unary terminal response, SSE events, upstream status/selected headers and whether dispatch began. Do not persist credentials or log bodies/tokens. Disable Pi retries and WS fallback for this HTTP slice. Validate input, bound bodies/events, propagate cancellation, and prevent arbitrary URL/path/credential exfiltration via redirect. Unsupported proxy/operation/protocol requirements must fail before dispatch.

A passing local fixture is bridge evidence only, not live OpenAI compatibility proof.

## Task 3: Go execution integration vertical slice

Inspect `internal/execution/cpa/{adapter,codex_provider}.go`, `internal/subscription/providers/codex/codex.go`, `internal/channel/modules/codex.go`, `internal/provideradapter/registry.go`, `internal/container/container.go` and platform config. Add an explicit driver seam without duplicating account management or changing defaults. Use the existing runtime credential preparation and policy layers. Connect the pi bridge with bounded HTTP transport and cancellation. Never falsely register CPA's broad native/converted/WS capabilities for Pi; unverified routes stay unavailable on Pi. Driver identity must be visible in diagnostics. Add Go tests for explicit selection/default CPA, unsupported-before-dispatch, no fallback, transport errors, credential isolation and sidecar unavailable. If enabling Pi violates the full existing channel contract, keep the prototype opt-in/development-only rather than weaken validation.

## Task 4: Parity expansion, acceptance and safety gates

For each baseline capability, add driver-independent behavioral tests and run both drivers where possible. Preserve parallel tool calls, call IDs, argument deltas, reasoning/signatures, cache token accounting, error/terminal distinctions, limits, proxy policy and cancellation. Validate native WS separately (no HTTP fallback or business replay, identity fixed per connection). Add every missing auth, observation, count, image, search, conversion and subscription provider capability before production parity can pass. Existing unsupported baseline capabilities remain unsupported.

## Task 5: Packaging and documentation

Provide reproducible Node installation, local-only launch configuration, explicit bridge authentication, no auto-download at runtime, health/version checks, failure semantics and rollback. Preserve existing Docker/native paths; do not make CPA-only users depend on Node. Documents must explicitly distinguish prototype/native verified paths, unverified paths, unsupported paths and real-account verification.

## Task 6: Verification and publication

Run npm bridge tests against the actual installed pi-ai and local fake upstream. Run focused Go tests with GOMAXPROCS=2 and -p=2 (no Docker); run existing regression suites and builds as resources permit. Review spec compliance then code quality. Inspect git diff and secrets, commit using okbexx/okbexx@gmail.com, push only the fork feature branch and read back exact commit. Do not create upstream PR or claim parity until all inventory entries have evidence and authorized live tests pass. Record blocked/failed/not-run separately from passing.
