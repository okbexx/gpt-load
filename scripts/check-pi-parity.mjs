#!/usr/bin/env node
// Evidence gate only: never translates unknown support into a parity claim.
import { readFileSync, realpathSync } from 'node:fs';
import { dirname, isAbsolute, relative, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

const statuses = new Set(['unverified', 'unknown', 'in_progress', 'implemented', 'verified', 'unsupported', 'preserved_unsupported']);
const safeRelativePath = p => typeof p === 'string' && p.length > 0 && !isAbsolute(p) && !p.split(/[\\/]/).includes('..');

const object = value => value !== null && typeof value === 'object' && !Array.isArray(value);
const strings = value => Array.isArray(value) && value.every(x => typeof x === 'string' && x.length > 0) && new Set(value).size === value.length;
const sameSet = (a, b) => a.length === b.length && a.every(x => b.includes(x));
function requireTrue(obj, key) {
  if (key in obj && obj[key] !== true) throw new Error(`${key} must be true`);
}

function validateSummaries(inventory) {
  const caps = inventory.capabilities;
  const routes = caps.filter(c => c.category === 'channel_route');
  const channels = inventory.channels;
  if (channels !== undefined) {
    if (!object(channels)) throw new Error('channels must be an object');
    const names = [...new Set(caps.filter(c => c.channel !== 'shared').map(c => c.channel))];
    if (!sameSet(Object.keys(channels), names)) throw new Error('channels do not match capability channels');
    for (const [name, channel] of Object.entries(channels)) {
      if (!object(channel) || !['subscription', 'api_key'].includes(channel.connection) || !strings(channel.route_ids) ||
          !sameSet(channel.route_ids, routes.filter(c => c.channel === name).map(c => c.id))) throw new Error(`invalid channel route_ids or connection: ${name}`);
    }
  }
  if (inventory.route_coverage !== undefined) {
    const coverage = inventory.route_coverage;
    const names = channels ? Object.keys(channels) : [...new Set(routes.map(c => c.channel))];
    if (!object(coverage) || !sameSet(Object.keys(coverage), names)) throw new Error('route_coverage channel mismatch');
    for (const [name, value] of Object.entries(coverage)) {
      const count = routes.filter(c => c.channel === name).length;
      if (!object(value) || value.source_route_declarations !== count || value.inventory_route_records !== count) throw new Error(`route_coverage mismatch: ${name}`);
    }
  }
  if (inventory.counts !== undefined) {
    if (!object(inventory.counts)) throw new Error('counts must be an object');
    const expected = {
      capabilities: caps.length, baseline_supported: caps.filter(c => c.baseline_supported).length,
      baseline_unsupported: caps.filter(c => !c.baseline_supported).length,
      pi_verified: caps.filter(c => (c.pi_status === undefined ? c.pi?.verification : c.pi_status) === 'verified').length,
      channel_routes: routes.length,
    };
    if (channels) Object.assign(expected, {
      channels: Object.keys(channels).length,
      subscription_channels: Object.values(channels).filter(c => c.connection === 'subscription').length,
      api_channels: Object.values(channels).filter(c => c.connection === 'api_key').length,
      subscription_routes: routes.filter(c => channels[c.channel].connection === 'subscription').length,
    });
    for (const [key, value] of Object.entries(inventory.counts)) {
      if (key === 'by_category') {
        const categories = {};
        for (const cap of caps) {
          if (typeof cap.category !== 'string' || !cap.category) throw new Error('category required for by_category');
          categories[cap.category] = (categories[cap.category] ?? 0) + 1;
        }
        if (!object(value) || !sameSet(Object.keys(value), Object.keys(categories)) || Object.entries(value).some(([k, v]) => v !== categories[k])) throw new Error('counts.by_category mismatch');
      } else if (!(key in expected) || !Number.isSafeInteger(value) || value !== expected[key]) throw new Error(`counts.${key} mismatch or missing relationship`);
    }
  }
  if (inventory.release_gate !== undefined) {
    const gate = inventory.release_gate;
    if (!object(gate) || !['blocked', 'ready'].includes(gate.full_pi_release) || typeof gate.required_rule !== 'string' || !gate.required_rule.trim()) throw new Error('invalid release_gate');
    for (const key of ['no_silent_cpa_fallback', 'partial_opt_in_must_be_labelled', 'baseline_and_pi_verified_independently']) requireTrue(gate, key);
  }
}

export function evaluateParity(inventory, loadEvidence = () => { throw new Error('evidence reader unavailable'); }) {
  if (!object(inventory) || !/^[0-9a-f]{40}$/.test(inventory.baseline_commit ?? '')) throw new Error('baseline_commit must be a pinned SHA');
  requireTrue(inventory, 'requires_live_verification');
  if ('schema_version' in inventory && inventory.schema_version !== 1) throw new Error('unsupported schema_version');
  if (!Array.isArray(inventory.capabilities) || inventory.capabilities.length === 0) throw new Error('empty capability inventory');
  const ids = new Set();
  const blockers = [];
  let required = 0;
  let verified = 0;
  for (const cap of inventory.capabilities) {
    if (!object(cap) || typeof cap.id !== 'string' || !cap.id) throw new Error('capability id is required');
    if (ids.has(cap.id)) throw new Error(`duplicate capability id: ${cap.id}`);
    ids.add(cap.id);
    if (typeof cap.baseline_supported !== 'boolean') throw new Error(`baseline_supported must be explicit: ${cap.id}`);
    const piStatus = cap.pi_status === undefined ? cap.pi?.verification : cap.pi_status;
    if (!statuses.has(piStatus)) throw new Error(`invalid pi_status: ${cap.id}`);
    requireTrue(cap, 'requires_live_verification');
    if (cap.pi !== undefined && (!object(cap.pi) || (cap.pi.verification !== undefined && cap.pi.verification !== piStatus) || ('silent_cpa_fallback_allowed' in cap.pi && cap.pi.silent_cpa_fallback_allowed !== false))) throw new Error(`inconsistent pi metadata: ${cap.id}`);
    if ('release_gate_required' in cap && cap.release_gate_required !== cap.baseline_supported) throw new Error(`release_gate_required mismatch: ${cap.id}`);
    if (cap.baseline !== undefined && (!object(cap.baseline) || !['supported', 'unsupported'].includes(cap.baseline.support) || (cap.baseline.support === 'supported') !== cap.baseline_supported)) throw new Error(`baseline.support mismatch: ${cap.id}`);
    if ('pi_supported' in cap && cap.pi_supported !== null && typeof cap.pi_supported !== 'boolean') throw new Error(`invalid pi_supported: ${cap.id}`);
    if (piStatus === 'verified' && 'pi_supported' in cap && cap.pi_supported !== true) throw new Error(`verified pi_supported must be true: ${cap.id}`);
    const live = cap.live_verification;
    if (live !== undefined) {
      if (!object(live) || !['not_run', 'in_progress', 'failed', 'verified'].includes(live.status)) throw new Error(`invalid live_verification: ${cap.id}`);
      if (live.independent_of_baseline_required !== true) throw new Error(`independent_of_baseline_required must be true: ${cap.id}`);
      if (live.receipt !== null && (typeof live.receipt !== 'string' || !live.receipt)) throw new Error(`invalid live_verification.receipt: ${cap.id}`);
    }
    for (const [label, value] of [['pi_evidence', cap.pi_evidence], ['pi.evidence', cap.pi?.evidence]]) {
      if (value !== undefined && !strings(value)) throw new Error(`${label} must be an array of unique non-empty strings: ${cap.id}`);
    }
    if (cap.pi_evidence !== undefined && cap.pi?.evidence !== undefined && !sameSet(cap.pi_evidence, cap.pi.evidence)) throw new Error(`inconsistent Pi evidence aliases: ${cap.id}`);
    if (!cap.baseline_supported) continue;
    required++;
    let reason = '';
    if (piStatus !== 'verified') {
      reason = `Pi support is ${piStatus}`;
    } else if ((!Array.isArray(cap.pi_evidence) || cap.pi_evidence.length === 0) && (!Array.isArray(cap.pi?.evidence) || cap.pi.evidence.length === 0)) {
      reason = 'missing Pi verification evidence';
    } else {
      const evidencePaths = cap.pi_evidence ?? cap.pi.evidence;
      const kinds = new Set();
      for (const path of evidencePaths) {
        if (!safeRelativePath(path)) { reason = 'invalid evidence path'; break; }
        try {
          const receipt = loadEvidence(path);
          if (!object(receipt) || receipt.driver !== 'pi' || receipt.baseline_commit !== inventory.baseline_commit ||
              receipt.status !== 'passed' || !strings(receipt.capability_ids) || !receipt.capability_ids.includes(cap.id) ||
              !['contract', 'live'].includes(receipt.kind) ||
              (receipt.kind === 'live' && (receipt.authorized !== true || receipt.independent_of_baseline !== true))) {
            reason = 'mismatched or failed Pi evidence'; break;
          }
          kinds.add(receipt.kind);
          if (path === live?.receipt && receipt.kind !== 'live') { reason = 'live_verification receipt is not live evidence'; break; }
        } catch {
          reason = 'unreadable Pi verification evidence'; break;
        }
      }
      if (!reason && !kinds.has('contract')) reason = 'missing Pi contract evidence';
      if (!reason && !kinds.has('live')) reason = 'missing authorized live-account evidence';
      if (!reason && (live?.status !== 'verified' || !evidencePaths.includes(live.receipt))) reason = 'missing or inconsistent live_verification receipt';
    }
    if (reason) blockers.push({ id: cap.id, channel: cap.channel, reason });
    else verified++;
  }
  validateSummaries(inventory);
  if (inventory.release_gate?.full_pi_release === 'ready' && blockers.length) throw new Error('release_gate claims ready with blocked capabilities');
  if (required === 0) throw new Error('inventory contains no baseline-supported capabilities');
  return { baseline_commit: inventory.baseline_commit, total: ids.size, required, verified, ready: blockers.length === 0, blockers };
}

function main() {
  const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
  const path = process.argv[2] ? resolve(process.argv[2]) : resolve(root, 'docs/pi-driver/capabilities.json');
  try {
    const inventory = JSON.parse(readFileSync(path, 'utf8'));
    const result = evaluateParity(inventory, evidencePath => {
      const resolved = realpathSync(resolve(root, evidencePath));
      const rel = relative(realpathSync(root), resolved);
      if (rel === '..' || rel.startsWith(`..${sep}`) || isAbsolute(rel)) throw new Error('evidence escapes repository');
      return JSON.parse(readFileSync(resolved, 'utf8'));
    });
    console.log(JSON.stringify(result, null, 2));
    process.exitCode = result.ready ? 0 : 1;
  } catch (error) {
    console.error(`Pi parity inventory rejected: ${error.message}`);
    process.exitCode = 2;
  }
}
if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) main();
