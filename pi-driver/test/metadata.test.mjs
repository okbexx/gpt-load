import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { safeMetadata, retrySeconds, MAX_RETRY_SECONDS } from '../metadata.mjs';

// Shared synthetic inputs exercise the Node filter, the Go filter, and the
// second filtering pass at the bridge boundary. No source-text assertions.
const cases = JSON.parse(readFileSync(new URL('../../integration/pi/testdata/metadata-contract.json', import.meta.url), 'utf8'));
for (const { name, input, expected } of cases) {
  test(`metadata contract: ${name}`, () => {
    const result = safeMetadata(new Headers(input));
    assert.deepEqual(result, expected);
    assert.deepEqual(safeMetadata(new Headers(result)), result);
  });
}

test('HTTP-date retry delay normalization uses a fixed clock and strict syntax', () => {
  const now = Date.parse('Sat, 19 Sep 2026 00:00:00 GMT');
  assert.equal(retrySeconds('Sat, 19 Sep 2026 00:00:17 GMT', now), 17);
  assert.equal(retrySeconds('Fri, 18 Sep 2026 23:59:59 GMT', now), 0);
  assert.equal(retrySeconds(new Date(now + MAX_RETRY_SECONDS * 1000).toUTCString(), now), MAX_RETRY_SECONDS);
  assert.equal(retrySeconds(new Date(now + (MAX_RETRY_SECONDS + 1) * 1000).toUTCString(), now), undefined);
  for (const malformed of ['2026-09-19', 'Sun, 19 Sep 2026 00:00:17 GMT', 'Sat, 19 Sep 2026 00:00:17 GMT SECRET', '17.0']) {
    assert.equal(retrySeconds(malformed, now), undefined);
  }
});
