# Independent Pi driver delivery Implementation Plan

> **For Hermes:** Use subagent-driven-development skill to implement this plan task-by-task.

**Goal:** A usable explicitly selected Pi driver that preserves the selected channel's complete baseline capability contract, without using CPA execution to fill gaps.

**Architecture:** GPT-Load keeps scheduling, encrypted credential storage, usage accounting and operator controls. Pi owns selected upstream execution. Pure protocol codecs may be reused as libraries, but CPA executors, implicit fallback and direct transports mislabeled as Pi are not substitutes for Pi execution. Missing Pi primitives require an explicit Pi extension and their own behavioral evidence.

**Tech Stack:** Existing Go gateway and pinned pi-ai Node sidecar; real local HTTP/TLS/WS upstream fixtures; repository-pinned Go toolchain.

## Confirmed acceptance boundary

- The user explicitly selected independent Pi execution, not mixed routing.
- Only isolated testing is authorized for now. No production credentials or paid/provider calls. A dedicated test account will be supplied later when offline confidence is sufficient.
- A working native HTTP slice or preservation of the default CPA driver is not final delivery.
- A green local test run is not live-provider parity. Keep the full release gate blocked until the separately authorized live validation is complete.
- Never drop original request fields silently to make a converted protocol pass. Retain baseline fidelity rejections.

## Work sequence and proof

### 1. Close confirmed unary failure defects

Files: `internal/subscription/providers/codex/pi.go`, `pi_test.go`, `internal/execution/cpa/adapter.go`, dedicated Pi failure tests.

1. Reproduce a failed/incomplete result followed by done being accepted.
2. Reject non-completed unary native results at the Go boundary, independently of Node.
3. Preserve safe driver/response metadata on post-header unary failure without HTTP-200 success or replay-safe claims.
4. Run focused packages and real tagged integration. Preserve default CPA behavior.

### 2. Converted inference vertical slice

Files: `internal/subscription/providers/codex/pi_conversion.go`, its tests, `internal/execution/cpa/pi.go`, and `pi_conversion_integration_test.go`.

1. Verify the actual pinned pure codec request/response contract.
2. Add OpenAI Chat, Anthropic Messages and Gemini inference conversion around the native Pi executor. Conversion code must never call CPA transport.
3. Keep native Responses unchanged; count/images/search remain rejected until independently implemented.
4. Prove streaming and unary tools, finish semantics, usage, errors and cancellation through real Adapter → encrypted credential manager → Go bridge → Node/pi-ai → trusted local TLS upstream.
5. Assert zero CPA execution calls and actual Pi originator/upstream request.

### 3. Independent local token estimation

Preserve the baseline's restricted-text estimator semantics and supported model/field limits for Responses, Anthropic and Gemini count routes. Do not use a model generation request as token counting, nor call CPA CountTokens as a fallback. Verify estimator equivalence with baseline fixtures and no network/credential preparation.

### 4. Per-attempt network policy

Preserve direct, inherited/environment and explicit proxy choices across HTTP and subsequently WS/control operations. Isolate account proxy policy per attempt; no process-global env mutation. Test HTTP(S)/SOCKS5 boundaries required by the baseline, authenticated proxies, cancellation, redirects, TLS verification and no-direct-fallback behavior.

### 5. Codex native WebSocket

Provide handshake, prewarm, multi-turn continuation, identity fixed per connection, terminal/error/close semantics, quota observation and bounded cancellation. Never inherit SDK automatic SSE fallback or business-history replay. Verify actual local WebSocket traffic with zero HTTP fallback.

### 6. Codex search and images

Preserve dedicated search semantics, image generation/edit JSON/multipart behavior, usage and supported streaming boundaries. Pi lacks these built-ins in the pinned release; implement explicit provider extensions rather than disguising a generic proxy as pi-ai capability. Test lossless supported inputs and rejection of unsupported combinations.

### 7. Subscription lifecycle and remaining channels

Inventory OAuth/import/refresh/identity, model discovery, quota/reset and the Claude/Antigravity/Grok execution contracts separately. Shared GPT-Load control ownership is not proof of an independent Pi implementation. In particular, pinned Pi has no built-in Antigravity and its xAI subscription endpoint differs from the baseline Grok CLI proxy. Each mismatch needs an explicit implementation and independent tests before being marked supported.

### 8. Packaging and whole-system offline acceptance

Provide reproducible startup, readiness/version verification, installation and rollback while CPA-only deployments remain independent of Node. Exercise management setup and client-facing HTTP gateway, not only adapters; verify accounting/logging/affinity and error delivery. Run all applicable original regressions and distinguish environment failures from regressions. Record exact coverage, not a synthetic total marketed as full parity.

### 9. Authorized live acceptance (blocked by user until ready)

Only after offline coverage and reviews are sufficient, obtain a dedicated account and explicit bounds. Verify the real provider behavior for supported capabilities; never create live receipts from fixtures. Release only after all required evidence gates pass.

## Verification commands

- `npm --prefix pi-driver test`
- `node --test scripts/check-pi-parity.test.mjs`
- `GO_BIN=/path/to/pinned/go bash scripts/verify-pi-driver.sh`
- `go test -race -p=2 -count=3 -tags=pi_integration ./internal/execution/cpa ./integration/pi ./internal/subscription/providers/codex`
- `go test -p=2 -count=1 . ./internal/...`

Go runs use bounded parallelism and the already-installed pinned toolchain/module cache. No Docker installs, production access or dependency/toolchain auto-downloads are part of these isolated checks.
