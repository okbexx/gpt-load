# Optional Pi Codex HTTP driver — vertical slice

This standalone Node.js sidecar **really invokes** the published `@earendil-works/pi-ai` Codex Responses implementation, exact-pinned to **0.85.1** with an npm lockfile. It is not a direct proxy relabeled Pi. No Go integration, CPA/Bifrost change, tool execution, automatic account selection, or full capability parity is claimed here.

## Run

Node **22.22.3** is the tested runtime (Node 22 only in this package's engine range).

```sh
cd pi-driver
npm ci --ignore-scripts --no-audit --no-fund
npm test
# Inject a randomly generated sidecar secret through your secret manager:
PI_DRIVER_TOKEN='<at least 32 printable non-space characters>' npm start
# Optional server-owned JSON configuration:
PI_DRIVER_TOKEN='<secret>' node server.mjs --config config.local.json
```

Example configuration (do not include credentials):

```json
{"host":"127.0.0.1","port":8788,"allowedBaseUrls":[],"limits":{"concurrent":8,"timeoutMs":120000}}
```

Binding must explicitly be `127.0.0.1` or `::1`. Both `GET /health` and `POST /v1/execute` require `Authorization: Bearer <PI_DRIVER_TOKEN>`. Keep the sidecar on a trusted host: loopback auth does not protect against a privileged local process. No credentials or upstream error text are logged. SIGTERM/SIGINT close connections and cancel requests.

## Protocol v1

POST JSON:

```json
{
  "version":1,"provider":"codex","format":"openai-response","operation":"responses",
  "model":"your-codex-model","stream":true,
  "credential":{"access_token":"<OAuth access JWT>","account_id":"<matching account>"},
  "body":{"model":"your-codex-model","input":[{"role":"user","content":"Hello"}],"store":false},
  "headers":{},"base_url":"","proxy_url":"direct","session_id":"optional-session-id"
}
```

Credentials are supplied per request. Account ID must exactly equal `https://api.openai.com/auth.chatgpt_account_id` decoded with the **same atob semantics as pinned Pi**. This is claim consistency validation, **not JWT signature verification**; upstream authenticates the token. There is no refresh/login/credential persistence.

Admission failures are HTTP 4xx JSON `{type:"error",code,status,dispatch_state:"not_sent"}` (capacity overload is 503). Admitted execution is HTTP 200 `application/x-ndjson`, even when the eventual upstream call fails:

- `{type:"headers",driver:"pi",status:200,headers:{"content-type":"text/event-stream"},dispatch_state:"maybe_sent"}` once successful upstream headers arrive.
- Stream mode: `{type:"chunk",data:"<base64 original native SSE bytes>"}`. Concatenate decoded bytes; transport chunk boundaries are not meaningful.
- Unary mode: `{type:"result",response:{...full native completed response...}}` (no chunk frames).
- Successful end: `{type:"done",dispatch_state:"maybe_sent"}` only after both native successful terminal and Pi parser success.
- Failed end: `{type:"error",code:"stable_code",status:0,dispatch_state:"maybe_sent"}`. `status` is the actual upstream HTTP code when known, otherwise 0 after dispatch; admission status is its HTTP rejection code. No `done` follows an error.

`maybe_sent` is conservative: it is set immediately before the single network attempt, not a promise that upstream received anything. Never retry implicitly based on an error frame. A downstream disconnect may prevent delivery of a terminal frame.

## Fidelity and execution

The published export `@earendil-works/pi-ai/api/openai-codex-responses` owns request setup, account extraction, headers, compression, SSE parsing and normalized completion. `onPayload` replaces Pi's converted messages with the native body, preserving tool schemas, parallel calls, reasoning settings, multimodal inputs, signatures and unknown JSON fields. Only `model`, `stream:true`, and `store:false` are enforced. Body/envelope model or stream contradictions are rejected.

A per-request custom fetch captures the **original UTF-8 SSE events** with bounded, pull-driven reading and sends them before handing a private copy to Pi. CRLF is normalized only in Pi's copy because Pi 0.85.1's SSE parser recognizes LF separators. Pi's normalized messages never reconstruct the client response. Unknown native events and native terminal usage/output/extra fields survive. Unary output preserves the full JSON object, not original whitespace/number lexemes. JSON semantics, not byte-identical request serialization, are promised.

Capture ends at the first successful `response.completed` or `response.done` with `response.status:"completed"`; post-terminal trailers are not part of this slice. Missing terminals, malformed JSON/UTF-8, incomplete responses, and failed/error terminal events are errors. **Failure events are intentionally not forwarded verbatim**: native error payloads may contain credentials. Already-forwarded successful events cannot be retracted. Successful model output is opaque data and not secret-scanned.

Transport is pinned to SSE, `maxRetries:0`, with an independent duplicate-dispatch guard. There is no WebSocket attempt/fallback or account retry. Pi may zstd-compress the outgoing request on this Node release; local integration tests decode and assert that actual payload.

## Security and bounds

- Default upstream: `https://chatgpt.com/backend-api`; GPT-Load's trusted default root `https://chatgpt.com` (with optional trailing slash) normalizes to it. Pi appends `/codex/responses` (or `/responses` for a `/codex` base; a complete `/codex/responses` URL stays unchanged). Custom URLs use this **Pi base convention**, not an implicit `/backend-api` prefix; the full normalized base must appear in `allowedBaseUrls`.
- Custom bases require an **exact server-owned normalized URL allowlist**; HTTPS required except literal loopback HTTP for testing. URLs with credentials/query/fragment are rejected. The allowlist is a trust boundary: only add hosts you trust with OAuth credentials, including their DNS and path ownership.
- Redirects are never followed; response/error bodies on non-2xx are cancelled without reading or exposing them.
- Client header overrides are all rejected, including authorization/account/host overrides. Only content-type is forwarded as a safe upstream response header.
- Proxy URLs are unsupported; only empty/omitted/`direct` is admitted. No explicit proxy transport is configured.
- `previous_response_id`, `conversation`, `store:true`, and `background:true` are rejected before dispatch. Session ID is only a Pi HTTP session/cache header hint, never persisted state.
- Defaults: request 2 MiB, individual SSE event 2 MiB, total upstream body 16 MiB, outgoing NDJSON frame 3 MiB, entire request deadline 120 seconds, 8 concurrent admissions. All are configurable positive integer `limits` keys: `requestBytes`, `eventBytes`, `responseBytes`, `frameBytes`, `timeoutMs`, `concurrent`.
- Backpressure awaits downstream drain before releasing the corresponding event to Pi; no stream tee or unbounded event queue. Pi still accumulates its normalized output, bounded by the total upstream cap. Concurrency bounds aggregate per-request memory. This is a bounded vertical slice, not a production load benchmark.

## Verification and limits of evidence

`npm test` uses Node's test runner, the **actual installed pinned Pi library**, synthetic JWTs and local HTTP upstreams. No real account/token or paid upstream is used. Initial RED was captured as `ERR_MODULE_NOT_FOUND` before implementation; the real first integration then revealed Pi's zstd request compression, and the fake upstream was corrected to decode it.

This slice does **not** provide complete Codex/CPA parity: WebSocket/stateful continuation, proxies, OAuth refresh, arbitrary header forwarding, incomplete-response success, account scheduling, file/media lifecycle APIs, tools execution, durable sessions, and other providers/operations remain unsupported. Live ChatGPT compatibility and production throughput have not been verified. Installation uses `--ignore-scripts`; the lockfile pins transitives, but is not a supply-chain audit.
