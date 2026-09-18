# Pi local bridge v1 (development contract)

This bridge is an experimental single-attempt transport, not full GPT-Load parity. HTTP binds only numeric loopback. Authorization: Bearer <separate bridge secret>, never an upstream token. No secrets in logs. Health: GET /health (no credential values; reports driver pi, protocol 1, pinned pi-ai version). Execute: POST /v1/execute.

Request JSON:
```
{"version":1,"provider":"codex","format":"openai-response","operation":"responses","model":"model-id","stream":true,"credential":{"access_token":"...","account_id":"..."},"body":{},"headers":{},"base_url":"","proxy_url":"","session_id":""}
```

Only Codex native Responses HTTP is admitted by the first vertical slice. Non-empty proxy requirements, other operations/protocols, and unsupported stateful requirements must fail before network dispatch, not be ignored. Default upstream: https://chatgpt.com/backend-api. Custom base URL requires server-side allowlist; no redirects with credentials. Never accept caller overrides of authorization, account identity, host, cookies or hop-by-hop headers. No disk credential store, OAuth refresh, account selection, retries or WS/HTTP fallback in the sidecar. Go owns lifecycle policies.

Successful admission returns Content-Type application/x-ndjson. Each line is one JSON frame:
- `{ "type":"headers", "driver":"pi", "status":200, "headers":{...}, "dispatch_state":"maybe_sent" }`: received upstream headers; only safe request IDs/retry-after/quota headers are exposed. Exactly one, before chunks.
- `{ "type":"chunk", "data":"<base64 raw SSE bytes>" }`: only for stream:true, native bytes preserving all response events and metadata; no synthesis from lossy Pi deltas.
- `{ "type":"result", "response":{...} }`: only for stream:false, full native terminal Responses object captured from upstream.
- `{ "type":"done", "dispatch_state":"maybe_sent" }`: valid completed terminal result observed. Exactly one terminal frame.
- `{ "type":"error", "code":"stable_safe_code", "status":0, "dispatch_state":"not_sent|maybe_sent" }`: terminal error; no raw messages/body/tokens. Before dispatch validation errors may instead return an HTTP 4xx JSON envelope with the same fields.

EOF without done/error is a transport failure. Failed/incomplete response events must not become done/success. Once upstream dispatch could have begun, sidecar failure is maybe_sent; transport ambiguity must never be marked safe to replay. Cancellation closes active upstream execution. Bound request size and NDJSON frame size; stream backpressure must not accumulate the whole response. Headers arriving from sidecar do not prove execution was successful. The Go client must reject invalid frame order and missing completion.

Implementation workers may tighten validation, but must coordinate changes to this contract through parent; no silent protocol drift.
