import http from 'node:http';
import { timingSafeEqual } from 'node:crypto';
import { once } from 'node:events';
import { safeMetadata, retrySeconds } from './metadata.mjs';
import { stream as codexStream } from '@earendil-works/pi-ai/api/openai-codex-responses';

const DEFAULT_BASE = 'https://chatgpt.com/backend-api';
const DEFAULT_LIMITS = { requestBytes: 2 * 1024 * 1024, eventBytes: 2 * 1024 * 1024,
  responseBytes: 16 * 1024 * 1024, frameBytes: 3 * 1024 * 1024, timeoutMs: 120000, concurrent: 8 };
class DriverError extends Error {
  constructor(code, status = 400) { super(code); this.code = code; this.status = status; }
}
const fail = (code, status) => { throw new DriverError(code, status); };
const object = x => x !== null && typeof x === 'object' && !Array.isArray(x);
export function normalizeBaseUrl(value) {
  if (typeof value !== 'string') fail('invalid_base_url');
  let u; try { u = new URL(value); } catch { fail('invalid_base_url'); }
  if (!['https:', 'http:'].includes(u.protocol) || u.username || u.password || u.search || u.hash) fail('invalid_base_url');
  if (u.protocol === 'http:' && !['127.0.0.1', '[::1]'].includes(u.hostname)) fail('insecure_base_url');
  const normalized = u.href.replace(/\/+$/, '');
  // GPT-Load's trusted Codex default is the origin, Pi needs /backend-api.
  return normalized === 'https://chatgpt.com' ? DEFAULT_BASE : normalized;
}
function validate(v, allowed) {
  if (!object(v) || v.version !== 1 || v.provider !== 'codex' || v.format !== 'openai-response' || v.operation !== 'responses') fail('unsupported_operation');
  if (typeof v.stream !== 'boolean' || typeof v.model !== 'string' || !v.model || v.model.length > 256 || /[\x00-\x1f\x7f]/.test(v.model)) fail('invalid_request');
  if (!object(v.body) || !('input' in v.body)) fail('invalid_body');
  if (v.body.model !== undefined && v.body.model !== v.model) fail('model_mismatch');
  if (v.body.previous_response_id != null || v.body.conversation != null || v.body.store === true || v.body.background === true) fail('unsupported_stateful_request');
  if (v.body.store !== undefined && v.body.store !== false) fail('invalid_body');
  if (v.body.background !== undefined && v.body.background !== false) fail('unsupported_stateful_request');
  if (v.body.stream !== undefined && v.body.stream !== v.stream) fail('stream_mismatch');
  if (v.proxy_url !== undefined && v.proxy_url !== '' && v.proxy_url !== 'direct') fail('unsupported_proxy');
  // No client header overrides in this slice. Pi owns auth and transport headers.
  if (v.headers !== undefined && (!object(v.headers) || Object.keys(v.headers).length)) fail('unsupported_headers');
  if (v.session_id !== undefined && (typeof v.session_id !== 'string' || !/^[\x21-\x7e]{0,128}$/.test(v.session_id))) fail('invalid_session_id');
  const c = v.credential;
  if (!object(c) || typeof c.access_token !== 'string' || c.access_token.length > 16384 || /\s/.test(c.access_token) || typeof c.account_id !== 'string' || !/^[\x21-\x7e]{1,256}$/.test(c.account_id)) fail('invalid_credential');
  try {
    const parts = c.access_token.split('.');
    if (parts.length !== 3 || JSON.parse(decodeBase64Url(parts[1]))?.['https://api.openai.com/auth']?.chatgpt_account_id !== c.account_id) fail('account_mismatch');
  } catch { fail('invalid_credential'); }
  const base = normalizeBaseUrl(v.base_url || DEFAULT_BASE);
  if (!allowed.has(base)) fail('upstream_not_allowed', 403);
  return { ...v, base };
}
async function readJSON(req, limit, signal) {
  return new Promise((resolve, reject) => {
    const parts = []; let bytes = 0;
    const cleanup = () => {
      req.off('data', data); req.off('end', end); req.off('error', error);
      signal.removeEventListener('abort', aborted);
    };
    const error = e => { cleanup(); req.pause(); reject(e); };
    const aborted = () => error(new DriverError('timeout', 408));
    const data = chunk => {
      bytes += chunk.length;
      if (bytes > limit) { error(new DriverError('request_limit', 413)); return; }
      parts.push(chunk);
    };
    const end = () => {
      cleanup();
      try { resolve(JSON.parse(Buffer.concat(parts).toString('utf8'))); }
      catch { reject(new DriverError('invalid_json')); }
    };
    req.on('data', data); req.once('end', end); req.once('error', error);
    signal.addEventListener('abort', aborted, { once: true });
    if (signal.aborted) aborted();
  });
}
function authorized(req, token) {
  const actual = Buffer.from(req.headers.authorization || '');
  const expected = Buffer.from(`Bearer ${token}`);
  return actual.length === expected.length && timingSafeEqual(actual, expected);
}
function errorFrame(code, status, dispatched) {
  return { type: 'error', code, status, dispatch_state: dispatched ? 'maybe_sent' : 'not_sent' };
}
function decodeBase64Url(segment) {
  if (typeof segment !== 'string' || !/^[A-Za-z0-9_-]+={0,2}$/.test(segment)) throw new Error('invalid base64url');
  const unpadded = segment.replace(/=+$/, '');
  const normalized = unpadded.replace(/-/g, '+').replace(/_/g, '/');
  return Buffer.from(normalized + '='.repeat((4 - normalized.length % 4) % 4), 'base64').toString('utf8');
}

// Pi 0.85.1 uses atob (not a Base64URL decoder) for local claim extraction.
// Give that parser a normalized copy ONLY; the fetch hook below always restores
// the byte-exact signed credential for the network Authorization header.
function piParserToken(token) {
  const parts = token.split('.');
  parts[1] = Buffer.from(parts[1], 'base64url').toString('base64');
  return parts.join('.');
}

// No tee(): one bounded, pull-driven reader sends original event bytes to the
// client before handing a copy to Pi. Pi's normalized events are never the wire.
function captureBody(body, state, limits, emit, streaming, signal) {
  const reader = body.getReader();
  let pending = Buffer.alloc(0), total = 0, ended = false;
  const decoder = new TextDecoder('utf-8', { fatal: true });
  const cancel = () => { void reader.cancel().catch(() => {}); };
  signal.addEventListener('abort', cancel, { once: true });
  state.cancel = cancel;
  return new ReadableStream({
    async pull(controller) {
      try {
        if (signal.aborted) throw signal.reason;
        if (state.terminal) { controller.close(); cancel(); return; }
        let match;
        while (!(match = /\r?\n\r?\n/.exec(pending.toString('latin1'))) && !ended) {
          const { value, done } = await reader.read();
          if (signal.aborted) throw signal.reason;
          ended = done;
          if (value) {
            total += value.byteLength;
            if (total > limits.responseBytes) fail('response_limit', 0);
            pending = Buffer.concat([pending, Buffer.from(value)]);
          }
          if (!/\r?\n\r?\n/.test(pending.toString('latin1')) && pending.length > limits.eventBytes) fail('response_limit', 0);
        }
        if (!pending.length && ended) { controller.close(); return; }
        const size = match ? match.index + match[0].length : pending.length;
        if (size > limits.eventBytes) fail('response_limit', 0);
        const raw = pending.subarray(0, size); pending = pending.subarray(size);
        const text = decoder.decode(raw);
        const data = text.split(/\r?\n/).filter(x => x.startsWith('data:')).map(x => x.slice(5).replace(/^ /, '')).join('\n');
        if (data && data.trim() !== '[DONE]') {
          let event; try { event = JSON.parse(data); } catch { fail('invalid_upstream_event', 0); }
          if (!object(event)) fail('invalid_upstream_event', 0);
          if (['error', 'response.failed', 'response.incomplete'].includes(event.type)) fail('upstream_failed', 0);
          if (['response.completed', 'response.done'].includes(event.type)) {
            if (!object(event.response) || event.response.status !== 'completed') fail('upstream_failed', 0);
            state.terminal = event.response;
          }
        }
        if (streaming) await emit({ type: 'chunk', data: raw.toString('base64') });
        // Pi 0.85.1 parses LF separators only. Normalize only its private copy.
        let copy = text.replace(/\r\n/g, '\n');
        if (!copy.endsWith('\n\n')) copy += '\n\n';
        controller.enqueue(new TextEncoder().encode(copy));
      } catch (e) {
        state.code ||= e instanceof DriverError ? e.code : (signal.aborted ? 'cancelled' : 'invalid_upstream_event');
        cancel(); controller.error(new Error(state.code));
      }
    },
    cancel() { signal.removeEventListener('abort', cancel); return reader.cancel().catch(() => {}); },
  }, { highWaterMark: 0 });
}

export function createDriver({ token, allowedBaseUrls = [], limits: overrides = {} } = {}) {
  if (typeof token !== 'string' || token.length < 32 || !/^[\x21-\x7e]+$/.test(token)) throw new Error('sidecar token must be at least 32 printable non-space characters');
  const limits = { ...DEFAULT_LIMITS, ...overrides };
  for (const [key, value] of Object.entries(limits)) if (!(key in DEFAULT_LIMITS) || !Number.isSafeInteger(value) || value <= 0) throw new Error('invalid limits');
  const allowed = new Set([DEFAULT_BASE, ...allowedBaseUrls.map(normalizeBaseUrl)]);
  let active = 0;
  const server = http.createServer({ maxHeaderSize: 16384 }, async (req, res) => {
    if (!['127.0.0.1', '::1', '::ffff:127.0.0.1'].includes(req.socket.remoteAddress) || !authorized(req, token)) {
      res.writeHead(401, { 'content-type': 'application/json', connection: 'close' }); res.end(JSON.stringify(errorFrame('unauthorized', 401, false))); return;
    }
    if (req.method === 'GET' && req.url === '/health') {
      res.writeHead(200, { 'content-type': 'application/json' }); res.end(JSON.stringify({ status: 'ok', driver: 'pi', version: 1 })); return;
    }
    if (req.method !== 'POST' || req.url !== '/v1/execute') {
      res.writeHead(404, { 'content-type': 'application/json' }); res.end(JSON.stringify(errorFrame('not_found', 404, false))); return;
    }
    if (active >= limits.concurrent) {
      res.writeHead(503, { 'content-type': 'application/json', connection: 'close' }); res.end(JSON.stringify(errorFrame('busy', 503, false))); return;
    }
    active++;
    const abort = new AbortController();
    const state = { dispatched: false, status: 0, code: null, terminal: null };
    const timer = setTimeout(() => { state.code = 'timeout'; abort.abort(); }, limits.timeoutMs);
    timer.unref();
    res.on('close', () => { if (!res.writableFinished) abort.abort(); });
    req.on('aborted', () => abort.abort());
    const emit = async frame => {
      if (res.destroyed || abort.signal.aborted) throw new Error('cancelled');
      const line = JSON.stringify(frame) + '\n';
      if (Buffer.byteLength(line) > limits.frameBytes) fail('response_limit', 0);
      if (!res.write(line)) await once(res, 'drain', { signal: abort.signal });
    };
    try {
      if (!/^application\/json(?:\s*;|$)/i.test(req.headers['content-type'] || '')) fail('unsupported_content_type', 415);
      const v = validate(await readJSON(req, limits.requestBytes, abort.signal), allowed);
      if (abort.signal.aborted) fail('timeout', 408);
      res.writeHead(200, { 'content-type': 'application/x-ndjson', 'cache-control': 'no-store' });
      res.flushHeaders();
      const model = { id: v.model, name: v.model, api: 'openai-codex-responses', provider: 'openai-codex', baseUrl: v.base,
        reasoning: true, input: ['text', 'image'], cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }, contextWindow: 0, maxTokens: 0 };
      const events = codexStream(model, { messages: [] }, {
        apiKey: piParserToken(v.credential.access_token), transport: 'sse', maxRetries: 0, signal: abort.signal,
        sessionId: v.session_id || undefined,
        onPayload: () => ({ ...v.body, model: v.model, stream: true, store: false }),
        fetch: async (url, init) => {
          if (state.dispatched) fail('duplicate_dispatch', 0);
          state.dispatched = true;
          const wireHeaders = new Headers(init?.headers);
          wireHeaders.set('authorization', `Bearer ${v.credential.access_token}`);
          const upstream = await fetch(url, { ...init, headers: wireHeaders, signal: abort.signal, redirect: 'manual' });
          state.status = upstream.status;
          if (!upstream.ok) state.retryAfter = retrySeconds(upstream.headers.get('retry-after'));
          if (!upstream.ok) { await upstream.body?.cancel(); state.code = upstream.status >= 300 && upstream.status < 400 ? 'upstream_redirect' : 'upstream_http_error'; throw new Error(state.code); }
          if (!/^text\/event-stream(?:\s*;|$)/i.test(upstream.headers.get('content-type') || '') || !upstream.body) {
            await upstream.body?.cancel(); state.code = 'invalid_upstream_content_type'; throw new Error(state.code);
          }
          const headers = safeMetadata(upstream.headers);
          // Restrict values as well as names: arbitrary error/request-id headers
          // are not trusted as a safe channel for credential-bearing text.
          headers['content-type'] = 'text/event-stream';
          await emit({ type: 'headers', driver: 'pi', status: upstream.status, headers, dispatch_state: 'maybe_sent' });
          return new Response(captureBody(upstream.body, state, limits, emit, v.stream, abort.signal), { status: upstream.status, headers: { 'content-type': 'text/event-stream' } });
        },
      });
      let piDone = false;
      // Drain Pi concurrently with pull-driven capture, so normalized deltas do
      // not accumulate in Pi's event queue. No agent/tool executor is involved.
      for await (const e of events) { if (e.type === 'done') piDone = true; }
      if (state.code || !piDone || !state.terminal) fail(state.code || (state.terminal ? 'pi_parse_error' : 'missing_terminal'), 0);
      if (!v.stream) await emit({ type: 'result', response: state.terminal });
      await emit({ type: 'done', dispatch_state: 'maybe_sent' });
      res.end();
    } catch (e) {
      const code = state.code || (e instanceof DriverError ? e.code : 'driver_error');
      const status = state.dispatched ? state.status : (e instanceof DriverError && e.status >= 400 ? e.status : 500);
      const frame = errorFrame(code, state.dispatched ? state.status : status, state.dispatched);
      if (state.retryAfter !== undefined) frame.retry_after_seconds = state.retryAfter;
      if (!res.destroyed) {
        if (!res.headersSent) res.writeHead(status >= 400 ? status : 500, { 'content-type': 'application/json', connection: 'close' });
        res.end(JSON.stringify(frame) + '\n');
      }
    } finally { clearTimeout(timer); state.cancel?.(); abort.abort(); active--; }
  });
  server.requestTimeout = limits.timeoutMs;
  server.headersTimeout = Math.min(limits.timeoutMs, 30000);
  const listen = server.listen.bind(server);
  server.listen = (...args) => {
    const host = typeof args[0] === 'object' ? args[0]?.host : args[1];
    if (!['127.0.0.1', '::1'].includes(host)) throw new Error('driver must bind an explicit loopback address');
    return listen(...args);
  };
  return server;
}
