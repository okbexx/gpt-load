#!/usr/bin/env node
// Evidence gate only: never translates unknown support into a parity claim.
import { readFileSync, realpathSync } from 'node:fs';
import { dirname, isAbsolute, relative, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

const statuses = new Set(['unverified', 'unknown', 'in_progress', 'implemented', 'verified', 'unsupported', 'preserved_unsupported']);
const safeRelativePath = p => typeof p === 'string' && p.length > 0 && !isAbsolute(p) && !p.split(/[\\/]/).includes('..');

export function evaluateParity(inventory, loadEvidence = () => { throw new Error('evidence reader unavailable'); }) {
  if (!inventory || !/^[0-9a-f]{40}$/.test(inventory.baseline_commit ?? '')) throw new Error('baseline_commit must be a pinned SHA');
  if ('requires_live_verification' in inventory && typeof inventory.requires_live_verification !== 'boolean') throw new Error('requires_live_verification must be boolean');
  if (!Array.isArray(inventory.capabilities) || inventory.capabilities.length === 0) throw new Error('empty capability inventory');
  const ids = new Set();
  const blockers = [];
  let required = 0;
  let verified = 0;
  for (const cap of inventory.capabilities) {
    if (typeof cap.id !== 'string' || !cap.id) throw new Error('capability id is required');
    if (ids.has(cap.id)) throw new Error(`duplicate capability id: ${cap.id}`);
    ids.add(cap.id);
    if (typeof cap.baseline_supported !== 'boolean') throw new Error(`baseline_supported must be explicit: ${cap.id}`);
    const piStatus = cap.pi_status ?? cap.pi?.verification;
    if (!statuses.has(piStatus)) throw new Error(`invalid pi_status: ${cap.id}`);
    if ('requires_live_verification' in cap && typeof cap.requires_live_verification !== 'boolean') throw new Error(`requires_live_verification must be boolean: ${cap.id}`);
    for (const [label, value] of [['pi_evidence', cap.pi_evidence], ['pi.evidence', cap.pi?.evidence]]) {
      if (value !== undefined && (!Array.isArray(value) || value.some(path => typeof path !== 'string' || path.length === 0))) throw new Error(`${label} must be an array of non-empty strings: ${cap.id}`);
    }
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
          if (receipt.driver !== 'pi' || receipt.baseline_commit !== inventory.baseline_commit ||
              receipt.status !== 'passed' || !Array.isArray(receipt.capability_ids) || !receipt.capability_ids.includes(cap.id) ||
              !['contract', 'live'].includes(receipt.kind)) {
            reason = 'mismatched or failed Pi evidence'; break;
          }
          kinds.add(receipt.kind);
        } catch {
          reason = 'unreadable Pi verification evidence'; break;
        }
      }
      if (!reason && !kinds.has('contract')) reason = 'missing Pi contract evidence';
      if (!reason && (cap.requires_live_verification || inventory.requires_live_verification) && !kinds.has('live')) reason = 'missing authorized live-account evidence';
    }
    if (reason) blockers.push({ id: cap.id, channel: cap.channel, reason });
    else verified++;
  }
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
