import test from 'node:test';
import assert from 'node:assert/strict';
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

test('preserved unsupported baseline is distinct from losing a supported capability', () => {
  const cap = capability('unsupported', { baseline_supported: false, pi_status: 'unsupported' });
  const supported = capability('responses', { pi_status: 'verified', pi_evidence: ['receipt.json'] });
  const receipt = { kind: 'contract', driver: 'pi', baseline_commit: baseline, status: 'passed', capability_ids: ['responses'] };
  const result = evaluateParity(inventory(cap, supported), () => receipt);
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
