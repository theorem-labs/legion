DONE_WITH_CONCERNS

# INFRA-143 U3: Tasks 11, 14, 16, 17, and 19

## Implemented

- `checks.py`
  - Enforces C1 with `ID_PATTERN.fullmatch` and the exact Python whitespace grammar.
  - Adds byte-based 4,032-byte subject validation.
  - Makes every hard-check record `{name, ok, detail, count, items}`; items are complete and C4-sorted.
  - Adds the hard refusal `contract_mapping_unique`, including both duplicate contract identifiers in its detail and items.
  - Adds complete call and address-taken symmetric-difference pairs, and complete field-offset misses plus per-function image evidence.
- `jsonio.py`
  - Adds C4 `canonical(value)`, exactly `json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False)`.
  - Adds the C3 `exclusion_document` canonical dedupe/sort and `{kind: {reason: count}}` aggregation.
- `link.py` and `structure.py`
  - Replace all former `repr` sort/dedupe keys with C4 canonical keys.
  - Change `link.build` to return `(graph, exclusions)`.
  - Move `unevaluated`, `image_decided`, `generic_unconfirmed`, and contract-functions-without-C detail out of graph analysis; graph retains `analysis.excluded` aggregate counts.
  - Aggregate optional unit-fact and image-fact `exclusions`; absent facts-side exclusions intentionally yield no items until U2/U4 integration.
  - Record link, structure, and callback drops in the C3 shape.
  - Detect duplicate contract mappings without changing `contracts.py`, because that file is outside U3 scope.
- `owners.py`
  - Assigns any per-CPU struct (`per_cpu: true`) `cross_cutting`.
- `report.py`
  - Accepts the exclusions document and renders counts by kind/reason plus the first ten canonical items for each reason.
- Tests
  - Add C1 valid/invalid mutation matrix, C4 canonical/key-order property, fresh-vs-cached fixture comparison, C3 fact/structure/callback coverage, complete-hard-check details, full pair/miss coverage, duplicate contract refusal, and per-CPU struct/subject-length tests.

## C3 kinds and reasons

- `call`: facts-side expression reasons (`sizeof`, `alignof`, `type`, `dead_branch`, `after_exit`, `ignored_argument`, `constant`, `generic_control`, `generic_unselected`, `builtin`); `image_decided`; `generic_unconfirmed`.
- `access`: the same facts-side expression reasons; `image_decided`; `generic_unconfirmed`; `unbound_parameter`; `parameter_at_root`; `record_unknown`.
- `embedding`: `record_not_a_node`.
- `membership`: `arguments_unresolved`, `end_not_a_node`.
- `callback`: `target_without_c_definition`, `no_c_initializer`, `evidence_not_a_table`, `kind_not_read`.
- Optional U2/U4 fact items pass through with their C3 kinds/reasons, including `callback_table` / `tokens_unavailable`, `function` / `result_registers_unknown`, and `image_call` / `target_not_text`.
- `contract_function`: `no_c_file`.

## Verification

All commands ran from `/home/sami/src/legion-cgraph-link`.

- Red test before production code: containerized `uv sync --frozen && uv run pytest -q tests/test_checks.py tests/test_graph.py tests/test_owners.py` failed as expected for missing C1 fullmatch, canonical serializer/C3 document, hard-check fields/pairs/misses, per-CPU role, and the new link result interface.
- Focused containerized regressions passed:
  - 5 passed for canonical C4, C3 callback classification, key-reversal graph property, and per-CPU role.
  - 6 passed after the check-layer changes.
  - 2 passed for optional facts-side exclusions and callback classifications.
  - 1 passed for full field-miss image evidence.
  - 1 passed for structure C3 drops after correcting the test input to the established `(function, call)` API.
- Final exact cgraph CI lane in the `ghcr.io/astral-sh/uv:python3.13-bookworm-slim` container:
  - `uv sync --frozen`: passed.
  - `uv run ruff check src tests`: passed.
  - `uv run ruff format --check src tests`: passed (`26 files already formatted`).
  - `uv run mypy`: stopped at the expected U1 integration boundary: 9 errors in `src/cgraph/pipeline.py`, because the pre-U1 writer still assigns the new `(graph, exclusions)` result to `graph` and passes that tuple to owners, measures, checks, report, and graph counts.
  - Therefore pytest was not reached by the chained CI command; the pipeline writer seam must land before the full lane can complete.
- `bash .github/scripts/check-private-names.sh`: passed with no findings.
- `git diff --check`: passed.

## Required U1 integration hook

`pipeline.run` must unpack `graph, exclusions = link.build(...)`; register `exclusions.json` in its existing atomic output map; place `exclusions["counts"]` in `checks.json`; and call `report.render(..., exclusions)`. The C3 output value is exactly:

```python
{"format": "cgraph/exclusions/v1", "part": part, "counts": {kind: {reason: count}}, "items": items}
```

The coordinator assigned CgraphIO the separate Task 17 CLI `--json` projection: it must omit full `items` and retain only summary/count fields. No second output writer was added here.

## Documentation and ledger

- README was intentionally untouched: no node-id content change was unavoidable in U3's allowed files. The parent should include the C1 owner-key alphabet and non-encoding statement if the README task is assigned elsewhere.
- Hardening ledger entries: none. No failing test was weakened, skipped, or deleted; no workaround was taken.
- Writes outside this workspace: none.
