// Fixed typed allowlist, mirrored by pi_metadata.go. Never copy opaque IDs,
// limit names, arbitrary namespaces or private upstream header values.
export const MAX_RETRY_SECONDS = 31622400; // 366 days; also bounds relative resets.
const MAX_RESET_AT = 253402300799; // Last second of year 9999.
const MAX_WINDOW_MINUTES = 527040;
const ACTIVE_LIMITS = new Set(['premium', 'codex', 'codex_bengalfox']);
const WINDOW = /^x-codex-(?:bengalfox-)?(?:primary|secondary)-(used-percent|window-minutes|reset-at|reset-after-seconds)$/;
const BOOLEAN = /^x-codex-(?:bengalfox-)?(?:allowed|limit-reached)$/;
const GENERIC = /^x-codex-(?:(?:primary|secondary)-|allowed$|limit-reached$|active-limit$)/;
function integer(value, max, min = 0) {
  if (typeof value !== 'string' || value.length > 16 || !/^(0|[1-9][0-9]*)$/.test(value)) return undefined;
  const n = Number(value);
  return Number.isSafeInteger(n) && n >= min && n <= max ? n : undefined;
}
export function retrySeconds(value, now = Date.now()) {
  const seconds = integer(value, MAX_RETRY_SECONDS);
  if (seconds !== undefined) return seconds;
  // Accept canonical IMF-fixdate only, not Date.parse's permissive arbitrary text.
  if (typeof value !== 'string' || value.length !== 29 || !/^(Mon|Tue|Wed|Thu|Fri|Sat|Sun), [0-9]{2} (Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) [0-9]{4} [0-9]{2}:[0-9]{2}:[0-9]{2} GMT$/.test(value)) return undefined;
  const date = Date.parse(value);
  if (!Number.isFinite(date) || new Date(date).toUTCString() !== value) return undefined;
  const delay = Math.max(0, Math.ceil((date - now) / 1000));
  return delay <= MAX_RETRY_SECONDS ? delay : undefined;
}
export function safeMetadata(headers) {
  const result = {};
  const active = headers.get('x-codex-active-limit');
  // Dropping an unknown active-limit must not relabel its generic window as
  // account quota. Likewise an unrecognized namespace makes attribution unsafe.
  const unsafeGeneric = (active !== null && !ACTIVE_LIMITS.has(active)) || [...headers.keys()].some(k =>
    /^x-codex-.+-(primary|secondary)-(used-percent|window-minutes|reset-at|reset-after-seconds)$/.test(k) && !WINDOW.test(k));
  for (const [key, value] of headers) {
    if (unsafeGeneric && GENERIC.test(key)) continue;
    if (key === 'retry-after') {
      const seconds = retrySeconds(value);
      if (seconds !== undefined) result[key] = String(seconds);
    } else if (key === 'x-codex-active-limit' && ACTIVE_LIMITS.has(value)) result[key] = value;
    else if (BOOLEAN.test(key) && ['true', 'false', '1', '0'].includes(value)) result[key] = value === 'true' || value === '1' ? 'true' : 'false';
    else {
      const field = WINDOW.exec(key)?.[1];
      if (!field) continue;
      if (field === 'used-percent') {
        if (value.length <= 16 && /^(0|[1-9][0-9]*)(\.[0-9]+)?$/.test(value) && Number(value) <= 100) result[key] = String(Number(value));
      } else {
        const max = field === 'window-minutes' ? MAX_WINDOW_MINUTES : field === 'reset-at' ? MAX_RESET_AT : MAX_RETRY_SECONDS;
        const n = integer(value, max, 1);
        if (n !== undefined) result[key] = String(n);
      }
    }
  }
  return result;
}
