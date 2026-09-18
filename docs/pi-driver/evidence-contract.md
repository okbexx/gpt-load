# Pi parity evidence contract

This document defines the machine-checked release-gate contract. It does **not** claim Pi parity. The current inventory remains blocked until every baseline-supported capability has both contract evidence and authorized live evidence.

## Inventory identity and rows

- `baseline_commit` is a lowercase, 40-character Git SHA. Baseline source evidence is kept in each row's `evidence`/`provenance`; it is not Pi evidence.
- `capabilities` is a non-empty array of unique rows. Every row explicitly sets `baseline_supported` and a valid `pi_status` (or its supported alias). When `release_gate_required` is supplied, it must equal `baseline_supported`; omitting it never removes a supported row from the gate.
- `pi_status` must be one of `unverified`, `unknown`, `in_progress`, `implemented`, `verified`, `unsupported`, or `preserved_unsupported`. `verified` is not sufficient by itself.
- If `pi` aliases `pi_status` or `pi.evidence`, aliases must agree. `baseline.support` must agree with `baseline_supported`; `pi_supported` is either `null` or boolean and must be `true` for a verified row.
- Existing records remain unverified. Preserving CPA/Bifrost or a baseline source receipt is not Pi evidence.

## Evidence receipts

A baseline-supported row must list `pi_evidence` (or the equivalent `pi.evidence`) with unique, non-empty repository-relative paths. Every path must resolve inside the repository; absolute paths, `..` traversal, and symlink escape are rejected.

Each receipt must be JSON with:

```json
{
  "kind": "contract",
  "driver": "pi",
  "baseline_commit": "<same inventory SHA>",
  "status": "passed",
  "capability_ids": ["<row id>"]
}
```

`contract` evidence proves the offline/schema/behavior contract only. It cannot release a row. `live` evidence additionally requires:

```json
{
  "authorized": true,
  "independent_of_baseline": true
}
```

The live receipt must be independently produced from the Pi path and must name the row. Do not fabricate receipts or check in production credentials. A mock, source inspection, CPA test, or generic HTTP 200 is not authorized live evidence.

## Per-row live verification

Every baseline-supported row must contain:

```json
"live_verification": {
  "status": "verified",
  "independent_of_baseline_required": true,
  "receipt": "path/to/the-live-receipt.json"
}
```

The referenced receipt must also appear in `pi_evidence`, have `kind: "live"`, and pass all receipt checks. A missing/unverified live record or unusable receipt blocks the capability (exit 1). Supplied malformed flags or contradictory schema aliases reject the inventory (exit 2). Full parity always requires contract **and** authorized live evidence; an omitted optional flag cannot weaken this rule, and a supplied `requires_live_verification` must be `true`.

## Relational inventory checks

When present, summaries are checked against records rather than frozen counts:

- `counts.capabilities`, support totals, `pi_verified`, route totals, channel totals, subscription/API totals, and `by_category` equal their derived values.
- `channels[*].route_ids` exactly matches the route capability IDs for that channel; channel keys and `route_coverage` keys match the inventory's channel set.
- `route_coverage[*].source_route_declarations` and `inventory_route_records` equal the corresponding route rows.
- `release_gate` must preserve the no-silent-fallback, labelled-partial-opt-in, and independent-verification booleans as `true`. A `ready` claim with blockers is malformed.

## CLI result codes

`scripts/check-pi-parity.mjs [inventory.json]` emits JSON when the inventory is valid:

- `0`: all required rows satisfy contract and live evidence requirements (`ready: true`)
- `1`: valid inventory, but one or more required rows are blocked; this includes absent, mismatched, unreadable, or out-of-repository evidence (including symlink escape)
- `2`: malformed/unreadable inventory or invalid summary/schema relationships

The checked-in inventory is intentionally in the blocked state; this gate must not be used to claim parity.
