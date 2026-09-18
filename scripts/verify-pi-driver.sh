#!/usr/bin/env bash
# Local/synthetic verification only. A green run is NOT a full-parity release.
set -euo pipefail
ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
GO_BIN="${GO_BIN:-go}"
command -v "$GO_BIN" >/dev/null || { printf '%s\n' 'Go is required; set GO_BIN to an installed Go executable.' >&2; exit 2; }
command -v node >/dev/null || { printf '%s\n' 'Node is required.' >&2; exit 2; }
command -v npm >/dev/null || { printf '%s\n' 'npm is required.' >&2; exit 2; }
node --input-type=module -e '
import { readFileSync } from "node:fs";
const wanted = JSON.parse(readFileSync("pi-driver/package.json", "utf8")).dependencies["@earendil-works/pi-ai"];
const installed = JSON.parse(readFileSync("pi-driver/node_modules/@earendil-works/pi-ai/package.json", "utf8")).version;
if (wanted !== installed) throw new Error("Run npm --prefix pi-driver ci --ignore-scripts before verification");
'
# Do not auto-install packages, start Docker, or create persistent build outputs.
# Dependencies and the Go toolchain must already be available locally.
export GOMAXPROCS=2
export GOPROXY=off
export GOTOOLCHAIN=local
TMP="$(mktemp -d)"
trap 'rm -rf -- "$TMP"' EXIT
printf '\n%s\n' '== Node sidecar and parity gate tests =='
npm --prefix pi-driver test
node --test scripts/check-pi-parity.test.mjs
printf '\n%s\n' '== Go regression tests =='
"$GO_BIN" test -p=2 -count=1 ./internal/channel ./internal/container ./internal/execution/cpa ./internal/subscription/providers/codex
printf '\n%s\n' '== Real Go -> Node -> pinned Pi; synthetic local upstream =='
"$GO_BIN" test -p=2 -count=1 -tags=pi_integration ./integration/pi ./internal/execution/cpa
printf '\n%s\n' '== Go vet and build =='
"$GO_BIN" vet -p=2 ./...
"$GO_BIN" build -p=2 -o "$TMP/gpt-load" .
printf '\n%s\n' '== Full-parity gate (blocked is expected until live verification) =='
PARITY_STATUS=0
node scripts/check-pi-parity.mjs > "$TMP/parity.json" || PARITY_STATUS=$?
if [[ "$PARITY_STATUS" -gt 1 ]]; then
  printf '%s\n' 'Parity gate failed: invalid inventory or execution error.' >&2
  exit "$PARITY_STATUS"
fi
node --input-type=module - "$TMP/parity.json" "$PARITY_STATUS" <<'JS'
import { readFileSync } from 'node:fs';
const result = JSON.parse(readFileSync(process.argv[2], 'utf8'));
const status = Number(process.argv[3]);
if (typeof result.ready !== 'boolean' || status !== (result.ready ? 0 : 1)) throw new Error('Parity gate status/result mismatch');
console.log(JSON.stringify({ total: result.total, required: result.required, verified: result.verified, ready: result.ready }));
console.log('LOCAL_SYNTHETIC_CHECKS=PASSED');
console.log(`FULL_PI_PARITY=${result.ready ? 'READY_BY_EVIDENCE_GATE' : 'BLOCKED'}`);
JS
