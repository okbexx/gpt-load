# Pi converted chat: isolated codec contract

## Execution ownership

`NewPiConvertedExecutor(native Executor)` wraps **a Pi native executor**. The
production composition is `NewPiConvertedExecutor(NewPiExecutor(...))` after
handling the latter constructor's error. Do not pass the CPA `NewExecutor`.
There is no alternate executor, CPA transport call, retry, or fallback in this
wrapper. Every admitted request calls the supplied native executor once.
Native `openai-response` requests bypass conversion unchanged.

This is **shared-codec reuse**, not independent codec implementation:
`github.com/router-for-me/CLIProxyAPI/v7/sdk/translator` and
`sdk/translator/builtin` from pinned v7.3.6 register the actual Codex wire
transforms. The existing embedded module also registers its service-tier codec
wrapper. No embedded execution API is called here. Registration is process-wide;
the wrapper refuses translation when plugin hooks are installed and verifies
that both request and response transforms exist instead of accepting the SDK's
identity fallback. Do not mutate registrations at runtime.

## Entry and integration boundary

- `ValidatePiConvertedRequest(q, stream)` performs pure preflight without an
  executor, credentials, or I/O. Use **after** the adapter's existing
  `prepareConvertedFidelity` / tool-allowlist preparation, before dispatch.
- Admitted client formats: `openai`, `claude`, `gemini`. Empty paths are admitted
  because GPT-Load's canonical chat bridge normally leaves them empty. Explicit
  paths must be the matching chat/messages/generateContent operation (Gemini
  accepts v1 and v1beta and requires streamGenerateContent in stream mode).
- `CountTokens` on this conversion wrapper remains explicitly unsupported;
  token counting is not silently substituted with CPA. Images, search, and
  non-chat paths are unsupported. This slice also rejects multimodal content
  blocks and non-function built-in tools; these need separate Pi capability work.
- Ordinary controls, including Claude's required `max_tokens`, temperatures,
  top-p, and Gemini generationConfig, are admitted as by the baseline codec.
  Admission does **not** imply Codex enforces every foreign-protocol option.
- Client `X-Api-Key`, `Anthropic-Version`, and `X-Goog-Api-Key` are consumed on
  a cloned header map, never forwarded to native execution. Configured custom
  headers are still rejected; other headers and all proxy/origin restrictions
  pass through the native Pi validator unchanged. Stateful server conversation,
  background, and persistent store requests remain unsupported.

Conversion retains original client bytes for reverse tool-name mapping and a
per-call response state object for streaming identity/arguments. The native
request is Responses at `/v1/responses`, with continuity key, base URL, proxy
policy, and remaining request metadata preserved. The native OriginalRequest is
in the native dialect; the client OriginalRequest is kept separately for the
response codec. Incoming model aliases must already be normalized by GPT-Load.

The wrapper sets native `stream` to the requested operation: the pinned Claude
and Gemini codecs force it true for CPA's transport, whereas Pi's unary bridge
already aggregates native completion. Unary Pi result objects are enclosed in a
real `response.completed` event envelope before invoking the non-stream codec.
Status, quota, request IDs, reasoning metadata, and upstream path are retained.

## Fidelity and known baseline limitations

Verified with the actual registered codecs (no fabricated conversion outputs):

- Unary parallel function calls: IDs, JSON arguments, finish reason, and usage
  for all three client formats.
- Request history: parallel calls and out-of-order results retain call IDs for
  all three formats. Long declared function names are restored using the
  original request rather than leaking Codex's shortened names.
- Streaming text and reasoning deltas, usage, and request-scoped state. OpenAI
  and Claude interleaved parallel tool arguments retain both tool identities.
  Gemini streaming uses the independent Pi codec and emits every adjacent and
  interleaved function call without the shared codec's single-slot loss.
- Common image inputs are admitted and preserved by native request codecs:
  OpenAI `image_url`, Claude base64/URL `image` blocks, and Gemini `inlineData`.
  Unsupported document, audio, file, and search forms remain rejected.
- Claude system instructions and Gemini systemInstruction remain developer
  instructions. The pinned Gemini source comment says "user", but its actual
  code emits `role:developer`; tests assert the actual output.

Known gaps are **not full protocol parity claims**:

1. Gemini unary conversion reads reasoning `content`, not Responses `summary`;
   summary-only reasoning is omitted by the baseline codec. Gemini streaming
   reasoning-summary deltas work. OpenAI/Claude unary summaries are covered.
2. Codex baseline deliberately does not forward Chat temperature/top-p/token
   limits and drops many foreign generation controls (including Claude
   max_tokens). These accepted no-op semantics are inherited, not newly
   implemented enforcement. OpenAI parallel_tool_calls is forced true by the
   pinned codec. Do not claim the controls are enforced upstream.
3. This wrapper does not promise preservation of every unknown extension or
   unsupported tool/content type. Adapter fidelity guards must remain enabled.
   Reasoning signatures and arbitrary structured-output variants need broader
   fixtures before claiming comprehensive parity.

## Stream/error contract

Native chunk bytes are **not assumed to be complete events**. A bounded SSE
assembler handles arbitrary fragmentation, coalesced events, CRLF, comments,
and multiple data lines. The codec receives one normalized `data:` payload at
a time. Per-event size is bounded by `piMaxFrameBytes`; malformed JSON, missing
type, truncation, unsuccessful terminal events, absent completion, and data
after completion fail rather than disappearing into a permissive codec.

Output is the codec's raw stream chunks (JSON for OpenAI/Gemini, SSE
for Claude). Gemini uses the independent Pi stream codec in
`pi_gemini_stream.go`; it keeps each function-call completion separate rather
than using the shared codec's single pending-output slot. The existing adapter
owns downstream SSE framing and final DONE / Gemini terminal normalization; do
not double-frame here. Native errors are
preserved unchanged. Converter failures have `maybe_sent`; admission failures
have `not_sent`. Cancellation unblocks the output consumer and cancels the
native child context, including when conversion fails or downstream stops.

## Isolated verification

Tests live in `internal/subscription/providers/codex/pi_conversion_test.go`.
They inject a captured native executor only; requests and responses use real
pinned codec registration. No external account, Node transport, network, or CPA
execution is exercised. RED was observed with missing constructor, then GREEN;
header-consumption tests also failed before the implementation and passed after.

```sh
GOMAXPROCS=2 GOCACHE=/home/jarl/.cache/gpt-load-pi/go-build \
GOMODCACHE=/home/jarl/.cache/gpt-load-pi/go-mod GOPROXY=off GOTOOLCHAIN=local \
/home/jarl/.cache/gpt-load-pi/toolchains/go/bin/go test \
  ./internal/subscription/providers/codex -run TestPiConverted -count=1
```

Real Pi/Node integration and adapter admission/routing integration are separate
verification gates. Passing this suite alone is not live-account or full-route
proof.
