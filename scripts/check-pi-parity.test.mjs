import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, writeFileSync, rmSync, symlinkSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawnSync } from 'node:child_process';
import { evaluateParity } from './check-pi-parity.mjs';

const baseline = 'a'.repeat(40);
const capability = (id, changes = {}) => ({ id, channel: 'codex', category: 'execution', baseline_supported: true, pi_status: 'unverified', evidence: ['source-reference'], ...changes });
const inventory = (...caps) => ({ baseline_commit: baseline, capabilities: caps });

test('malformed parity types are rejected as malformed, not ordinary gaps', () => {
  assert.throws(() => evaluateParity({ baseline_commit: baseline, requires_live_verification: 0, capabilities: [capability('x')] }), /requires_live_verification/);
  assert.throws(() => evaluateParity(inventory(capability('x', { pi_evidence: 'receipt.json', pi_status: 'verified' }))), /pi_evidence/);
  assert.throws(() => evaluateParity(inventory(capability('x', { pi: { verification: 'verified', evidence: [123] }, pi_status: undefined }))), /pi.evidence/);
});

test('unknown or partial Pi support cannot pass the release gate', () => {
  const result = evaluateParity(inventory(capability('responses'), capability('websocket', { pi_status: 'in_progress' })));
  assert.equal(result.ready, false);
  assert.deepEqual(result.blockers.map(x => x.id), ['responses', 'websocket']);
});

test('CPA preservation is not evidence of Pi parity', () => {
  const result = evaluateParity(inventory(capability('images', { baseline_preserved: true, pi_status: 'verified' })));
  assert.equal(result.ready, false);
  assert.match(result.blockers[0].reason, /evidence/);
});

test('duplicate IDs and empty manifests fail closed', () => {
  assert.throws(() => evaluateParity(inventory()), /empty/);
  assert.throws(() => evaluateParity(inventory(capability('same'), capability('same'))), /duplicate/);
});

test('missing baseline facts or unsupported status values cannot be excluded from totals', () => {
  assert.throws(() => evaluateParity(inventory({ id: 'bad', pi_status: 'unverified' })), /baseline_supported/);
  assert.throws(() => evaluateParity(inventory(capability('bad', { pi_status: 'maybe' }))), /pi_status/);
});

test('verified evidence must name Pi, matching baseline, capability and a passed result', () => {
  const cap = capability('responses', { pi_status: 'verified', pi_evidence: ['receipt.json'] });
  for (const wrong of [{ driver: 'cpa' }, { baseline_commit: 'b'.repeat(40) }, { status: 'failed' }, { capability_ids: ['different'] }]) {
    const receipt = { kind: 'contract', driver: 'pi', baseline_commit: baseline, status: 'passed', capability_ids: ['responses'], ...wrong };
    const result = evaluateParity(inventory(cap), () => receipt);
    assert.equal(result.ready, false);
  }
});

test('contract fixtures do not satisfy required live-account proof', () => {
  const cap = capability('responses', { pi_status: 'verified', pi_evidence: ['receipt.json'], requires_live_verification: true });
  const receipt = { kind: 'contract', driver: 'pi', baseline_commit: baseline, status: 'passed', capability_ids: ['responses'] };
  const result = evaluateParity(inventory(cap), () => receipt);
  assert.equal(result.ready, false);
  assert.match(result.blockers[0].reason, /live/);
});

const receipt = (kind, changes = {}) => ({ kind, driver: 'pi', baseline_commit: baseline, status: 'passed', capability_ids: ['responses'], ...(kind === 'live' ? { authorized: true, independent_of_baseline: true } : {}), ...changes });
const verifiedCap = () => capability('responses', {
  pi_status: 'verified', pi_evidence: ['contract.json', 'live.json'],
  live_verification: { status: 'verified', independent_of_baseline_required: true, receipt: 'live.json' },
});
const load = path => receipt(path === 'live.json' ? 'live' : 'contract');
const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const actual = () => JSON.parse(readFileSync(join(root, 'docs/pi-driver/capabilities.json'), 'utf8'));

test('contract-only claims without any opt-in live flag remain blocked', () => {
  const result = evaluateParity(inventory(capability('responses', { pi_status: 'verified', pi_evidence: ['contract.json'] })), load);
  assert.equal(result.ready, false);
  assert.match(result.blockers[0].reason, /live/);
});

test('live flags cannot disable the full-parity contract', () => {
  for (const value of [false, null, 0, 'true']) {
    assert.throws(() => evaluateParity({ ...inventory(capability('x')), requires_live_verification: value }), /requires_live_verification/);
    assert.throws(() => evaluateParity(inventory(capability('x', { requires_live_verification: value }))), /requires_live_verification/);
    assert.throws(() => evaluateParity(inventory(capability('x', { live_verification: { status: 'not_run', receipt: null, independent_of_baseline_required: value } }))), /independent_of_baseline_required/);
  }
});

test('both receipts and consistent row live verification are required', () => {
  assert.equal(evaluateParity(inventory(verifiedCap()), load).ready, true);
  for (const live of [undefined, { status: 'not_run', receipt: null, independent_of_baseline_required: true }, { status: 'verified', receipt: 'other.json', independent_of_baseline_required: true }]) {
    assert.equal(evaluateParity(inventory({ ...verifiedCap(), live_verification: live }), load).ready, false);
  }
  for (const wrong of [{ authorized: false }, { authorized: 'true' }, { independent_of_baseline: false }, { independent_of_baseline: undefined }, { driver: 'cpa' }]) {
    assert.equal(evaluateParity(inventory(verifiedCap()), path => path === 'live.json' ? receipt('live', wrong) : load(path)).ready, false);
  }
});

test('malformed optional metadata and contradictory aliases are rejected', () => {
  for (const changes of [
    { live_verification: false }, { live_verification: { status: 'maybe' } },
    { live_verification: { status: 'not_run', receipt: 1, independent_of_baseline_required: true } },
    { pi: false }, { pi: { verification: 'implemented' } }, { pi_supported: 'true' },
    { pi_status: null, pi: { verification: 'verified' } },
    { pi: { silent_cpa_fallback_allowed: true } },
    { release_gate_required: false }, { baseline: { support: 'unsupported' } },
    { pi_evidence: ['contract.json'], pi: { evidence: ['other.json'] } },
  ]) assert.throws(() => evaluateParity(inventory({ ...verifiedCap(), ...changes }), load));
});

test('actual inventory is internally consistent and remains blocked', () => {
  const doc = actual();
  const result = evaluateParity(doc);
  assert.equal(result.ready, false);
  assert.equal(result.total, doc.capabilities.length);
  assert.equal(result.required, doc.capabilities.filter(c => c.baseline_supported).length);
  assert.equal(result.verified, doc.counts.pi_verified);
  assert.equal(result.blockers.length, result.required - result.verified);
});

test('inventory summaries must agree with their records, not fixed snapshots', () => {
  const mutations = [
    d => d.counts.capabilities++, d => d.counts.baseline_supported++, d => d.counts.baseline_unsupported++,
    d => d.counts.pi_verified++, d => d.counts.channels++, d => d.counts.subscription_channels++,
    d => d.counts.api_channels++, d => d.counts.channel_routes++, d => d.counts.subscription_routes++,
    d => d.counts.by_category.channel_route++, d => delete d.counts.by_category.channel_route,
    d => d.counts.by_category.extra = 0, d => d.counts = null,
    d => d.channels.codex.route_ids.pop(), d => d.channels.codex.route_ids.push(d.channels.codex.route_ids[0]),
    d => d.channels.codex.route_ids[0] = d.channels.claude.route_ids[0],
    d => delete d.channels.codex, d => d.channels.codex.connection = 'typo',
    d => d.route_coverage.codex.source_route_declarations++,
    d => d.route_coverage.codex.inventory_route_records++, d => delete d.route_coverage.codex,
    d => d.route_coverage = [], d => d.channels = [],
    d => d.release_gate.no_silent_cpa_fallback = false,
  ];
  for (const mutate of mutations) {
    const doc = actual(); mutate(doc);
    assert.throws(() => evaluateParity(doc), undefined, mutate.toString());
  }
});

test('CLI uses repository-relative evidence, blocks symlink escape, and preserves 0/1/2 exits', t => {
  const inside = mkdtempSync(join(root, '.pi-gate-test-'));
  const outside = mkdtempSync(join(tmpdir(), 'pi-gate-'));
  t.after(() => { rmSync(inside, { recursive: true, force: true }); rmSync(outside, { recursive: true, force: true }); });
  const contractPath = relative(root, join(inside, 'contract.json'));
  const livePath = relative(root, join(inside, 'live.json'));
  writeFileSync(join(inside, 'contract.json'), JSON.stringify(receipt('contract')));
  writeFileSync(join(inside, 'live.json'), JSON.stringify(receipt('live')));
  const cap = { ...verifiedCap(), pi_evidence: [contractPath, livePath], live_verification: { ...verifiedCap().live_verification, receipt: livePath } };
  const run = doc => {
    const input = join(outside, 'inventory.json'); writeFileSync(input, JSON.stringify(doc));
    return spawnSync(process.execPath, [join(root, 'scripts/check-pi-parity.mjs'), input], { cwd: outside, encoding: 'utf8' });
  };
  const ready = run(inventory(cap));
  assert.equal(ready.status, 0, ready.stderr);
  assert.equal(JSON.parse(ready.stdout).ready, true);
  assert.equal(run(inventory(capability('pending'))).status, 1);
  assert.equal(run({ ...inventory(cap), requires_live_verification: false }).status, 2);
  writeFileSync(join(outside, 'live.json'), JSON.stringify(receipt('live')));
  rmSync(join(inside, 'live.json'));
  symlinkSync(join(outside, 'live.json'), join(inside, 'live.json'));
  const escaped = run(inventory(cap));
  assert.equal(escaped.status, 1);
  assert.match(JSON.parse(escaped.stdout).blockers[0].reason, /unreadable/);
});

test('baseline source paths and live-only receipts cannot replace Pi contract evidence', () => {
  const baselineOnly = { ...verifiedCap(), pi_evidence: [], evidence: ['contract.json', 'live.json'] };
  assert.equal(evaluateParity(inventory(baselineOnly), load).ready, false);
  const liveOnly = { ...verifiedCap(), pi_evidence: ['live.json'] };
  assert.match(evaluateParity(inventory(liveOnly), load).blockers[0].reason, /contract/);
  const wrongPointer = verifiedCap(); wrongPointer.live_verification.receipt = 'contract.json';
  assert.match(evaluateParity(inventory(wrongPointer), load).blockers[0].reason, /not live/);
});

test('preserved unsupported baseline is distinct from losing a supported capability', () => {
  const cap = capability('unsupported', { baseline_supported: false, pi_status: 'unsupported' });
  const result = evaluateParity(inventory(cap, verifiedCap()), load);
  assert.equal(result.ready, true);
  assert.equal(result.total, 2);
  assert.equal(result.required, 1);
  assert.equal(result.verified, 1);
});

test('evidence read errors and traversal references fail closed', () => {
  for (const path of ['missing.json', '../outside.json', '/tmp/outside.json']) {
    const result = evaluateParity(inventory(capability('responses', { pi_status: 'verified', pi_evidence: [path] })), () => { throw new Error('not found'); });
    assert.equal(result.ready, false);
  }
});
