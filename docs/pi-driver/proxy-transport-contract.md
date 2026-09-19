# Request-local Pi fetch transport

`pi-driver/proxy-transport.mjs` is a transport injection seam, **not a Pi executor**.
It never invokes CPA or selects another execution driver. Provider integration and
Go proxy admission remain separate work.

```js
import { createProxyFetch } from './proxy-transport.mjs';
const transport = createProxyFetch();
// selectedProxy must be null (explicit direct) or an admitted HTTP(S) proxy URL.
const fetchForThisRequest = (input, init) => transport(input, init, selectedProxy);
// Pass fetchForThisRequest into the actual Pi provider's supported fetch hook.
```

## Contract

- `createProxyFetch({ ca?, proxyCA = ca, connectTimeoutMs = 30000 }?)` returns
  `(input, init = {}, proxyURL) => Promise<Response>`. The third argument is
  mandatory; `undefined`, empty strings and unsupported schemes fail closed.
- `validateProxyURL(value)` returns a new URL or null, and throws a fixed error
  without including credentials. `http://`, `https://`, and `socks5://` authorities
  with optional `/` are accepted; `socks5://` is normalized privately to remote-DNS
  (`socks5h`) while the public URL remains unchanged. URL length is limited to 4096
  characters; decoded username/password to 1024 each (and SOCKS RFC byte fields to
  255). Whitespace, controls, backslashes, malformed
  credential escapes, paths, query, fragment and port zero are rejected.
- Uses Node's native fetch with a private, **single-use** dispatcher, backed by
  Node http/https and request-local agents. Direct mode explicitly uses a private
  agent, never the global dispatcher, environment proxy discovery or NO_PROXY.
  There are no environment/global-agent mutations, pools, retries, redirects or
  direct fallback. Caller `redirect` is intentionally overridden to `manual`.
- HTTP targets use forward proxy absolute-form requests; HTTPS targets use CONNECT.
  Both HTTP and HTTPS proxy endpoints are supported, including mixed schemes.
- Request bodies are streamed without reserialization, recompression or buffering
  the complete body. Native Request/Headers normalization applies. Authorization
  token values and precompressed body bytes remain unchanged in tested requests.
  Caller-supplied `Host`, `Proxy-Authorization` and `Proxy-Connection` are removed.
  Proxy Basic auth comes only from the selected URL. CONNECT contains no origin
  Authorization or request body. A forward HTTP proxy necessarily sees the origin
  HTTP request and is responsible for removing hop-by-hop proxy auth when forwarding;
  confidentiality from the proxy itself requires HTTPS to the origin.
- TLS certificate and hostname verification are mandatory for the TLS proxy and
  origin independently. No insecure switch is exposed. `ca`/`proxyCA` follow Node
  explicit CA semantics (they replace default trust for that connection).
- Native Response semantics include streaming SSE, response decompression, manual
  redirect responses, HEAD/204 null bodies, and normal HTTP error responses.
  Native fetch rejects proxy authentication status 407 as a transport failure.
- Socket, upload and agent cleanup happens on completion, signal abort, body
  cancellation, and transport error. Pending CONNECT sockets receive cancellation
  through agent connection options, not just `agent.destroy()` (which cannot see
  sockets still awaiting CONNECT). Transport errors and abort reasons are sanitized.
- SOCKS5 uses username/password handshake credentials only; origin authorization and
  body bytes are sent after successful negotiation. Destination hostnames are sent
  as SOCKS FQDNs for remote DNS, matching Go `x/net/proxy.SOCKS5`.
- `connectTimeoutMs` is a **time-to-response-headers** bound, including upload,
  connection establishment and CONNECT. Valid range: integer 1–300000. After
  headers, the caller owns the stream deadline via AbortSignal. Callers must
  consume the body or explicitly cancel it (including unread SSE bodies), or abort
  the request; merely dropping an unread infinite response is not deterministic
  resource cleanup. Native stream backpressure bounds transport buffering.

## Dependencies / support gaps

Verified with Node 22.22.3, installed `@earendil-works/pi-ai` 0.85.1 tree,
`http-proxy-agent` 7.0.2 and `https-proxy-agent` 7.0.6. Resolution is rooted at
`import.meta.resolve('@earendil-works/pi-ai')`, not another global npm tree.
No manifest, lockfile, runtime, container or install changes are part of this work.
These transitive dependencies must remain resolvable; a future manifest change
should declare them directly if they become an independently maintained contract.

**SOCKS5 is implemented:** `socks-proxy-agent` 8.x is resolved from the Pi driver
installation. Isolated loopback tests cover remote DNS, username/password
handshake, auth isolation, HTTP and HTTPS tunnels, abort/timeout cleanup and no
fallback. HTTP/2, WebSocket upgrades, NTLM, PAC and automatic environment proxy
selection are not supported by this bridge.
Native fetch dispatcher compatibility is tested on the repository's pinned Node
22 line; re-run this suite before moving Node major versions. No Pi provider
end-to-end or production deployment claim is made by these isolated tests.

## Local verification / reusable procedure

```sh
cd pi-driver
node --test test/proxy-transport.test.mjs
node --test
```

Tests use only loopback listeners and a temporary self-signed local CA certificate
created by the existing system OpenSSL; TLS verification remains enabled. They
exercise real installed proxy agents, HTTP forwarding, CONNECT+TLS, both mixed
schemes, untrusted certificates and hostname mismatch, independent proxy/origin
trust, byte-preserved compressed uploads, concurrent auth isolation, no fallback,
manual redirects, decompression, SSE, pre-abort/post-header abort, pending CONNECT
abort/timeout, unread cancellation, backpressure and empty-body semantics.
When testing proxy socket closure, make the fixture handle CONNECT socket `end`
by destroying its half-open server side; otherwise a client FIN can be mistaken
for an implementation leak. Never mock the agent or TLS verification to pass.

For SOCKS tunnels, upstream `close` must unpipe the client from that closed
destination and resume the client readable side. Otherwise pipe backpressure can
leave residual TLS `close_notify` bytes buffered and prevent observing the peer's
actual EOF. Do not destroy the client on upstream `close`: that would mask missing
transport shutdown. Assert the fixture's client socket set empties within two
seconds after the unary response, each stream abort/cancel, and each TLS rejection
separately, before test teardown can force-close sockets. For each stream abort or
cancel, also verify a live origin socket existed before cancellation and that the
origin's independently tracked socket set empties within the same two-second bound.
