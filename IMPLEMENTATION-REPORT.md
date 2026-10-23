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

# INFRA-143 U4, Task 20: the image facts cache, keyed on what decoding reads

Status of this unit: DONE_WITH_CONCERNS. The image cache cost is gone. It was 268 s under cProfile and is now 2.3 s, with byte-identical outputs. The warm `all` build still takes 575.9 s against the 436 s target. Almost all of what remains is `measures._interfaces`, which is U3's file. It is outside this unit, so this unit leaves it alone; see "The 436 s target".

Base: parent integration head `1d024227` in `/home/sami/src/legion-graph`, which already integrates U1 to U4.

## What changed

- `packages/cgraph/src/cgraph/imagecache.py`, rewritten.
  - The inputs digest is computed once per run. It covers the image and `System.map` SHA-256, the `[image]` table, the SHA-256 of `image.py` and of `imagecache.py`, and the capstone and pyelftools versions.
  - Before, `key()` hashed the whole-program `arity` and `results` maps (about 8,800 entries each) once for every function, 8,880 times per run.
  - Each function now has two immutable entries:
    - `image/callees/<symbol key>.json` holds the function's direct text callees. The symbol key is the inputs digest plus the symbol's name, address and size. These callees are a fact of the image alone.
    - `image/<facts key>.json` holds the facts. The facts key adds the only C facts that decoding reads for this function: its own result-register count, and `(name, arity)` for each direct callee. The coordinator chose this key (parent message, contract "minimal causally complete key").
  - Entries are compact canonical JSON (sorted keys). They are written once, through a temporary file and `os.replace`.
  - Any entry that cannot be read, is not JSON, or has the wrong shape raises `CacheError`, an `InputError` subclass. The CLI turns that into exit 3 with "remove the cache directory".
  - A disabled cache (no `--cache`) reads and hashes nothing.
- `packages/cgraph/src/cgraph/image.py`
  - `facts()` builds the symbol key, then reads the callee list, the facts key and the facts.
  - On a miss, it disassembles once. The same instruction list then yields both the facts and the direct-callee list, so the callee list cannot drift from what `_arguments` reads.
  - `_facts_from_cache` validates the exact field set and the element types, and returns `None` on a mismatch. The caller raises `CacheError` for that entry. Before, a wrong shape raised `KeyError`, which exited 3 as "internal error".
  - The constants `_DEFAULT_ARGUMENTS = 8` and `_DEFAULT_RESULTS = 2` replace the literals 8 and 2. The decoder and the key now read the same defaults.
- `pipeline.py` is unchanged. It already passes `cache_directory` to `Image`, as the parent wired it. `link.py` and `measures.py` (U3) are unchanged.
- `README.md`, "Incremental builds": documents the image cache key and the exit 3 on a corrupt entry. `AGENTS.md`, layout table: adds `imagecache.py`.

## Tests, written first

These ran on `linux28-poc-build` from a disposable copy. Red is the state before the implementation; it was run with the new tests against the `1d024227` code.

| Test | Red before the change | Green |
| --- | --- | --- |
| `test_image.py::test_image_cache_keys_a_function_on_its_own_results_and_its_direct_callees_arity` | `assert 1 == 0`: changing the unrelated `obj_free` arity and the `box_new` results decoded `bag_count` again | passes. Unrelated signature changes reuse the entry. A changed arity of the direct callee `walk`, or a changed own result count, decodes again. An absent signature is keyed by its default (8 arguments, 2 results). |
| `test_image.py::test_image_cache_key_covers_each_decoder_input` | `AttributeError` (no `symbol_key`/`facts_key`) | 13 distinct keys: image, map, rules, decoder source, name, address, size, results, callee arity, callee set, capstone version, pyelftools version |
| `test_image.py::test_image_cache_reuses_every_function_without_decoding` | no `callees/` entries | warm facts equal cold facts with a disassembler that raises when used. The cache bytes do not change. |
| `test_image.py::test_disabled_image_cache_does_not_read_inputs` | `AttributeError` (`enabled`) | passes |
| `test_build.py::test_warm_image_cache_gives_the_bytes_of_a_cold_build_without_decoding` | no `callees/` entries | `Cs` is replaced by a decoder that raises. The warm build parses nothing and decodes nothing, and its outputs equal the cold build's and an uncached build's, byte for byte. |
| `test_build.py::test_corrupt_image_cache_entry_refuses_without_changing_existing_outputs` (four cases: facts `{` and wrong shape; callees `{` and wrong shape) | the wrong-shape facts case exited 3 as `internal error: 'globals'` (a `KeyError`); the callee cases had no entries | each exits 3, names the image cache and "remove the cache directory", is not an internal error, and leaves the earlier outputs byte for byte |

Mutation checks, each applied to a separate VM copy and then reverted. The SHA-256 of `image.py` and `imagecache.py` was afterwards equal to the lane-tested tree.

- Facts key ignores callee arities: the scoping test fails with `assert 0 == 1` on the `walk` change, and the coverage test fails with `assert 11 == 13`.
- Callee list keeps only tail jumps (`j`, `c.j`) and drops `jal`: the scoping test fails with `assert 0 == 1`. A stale caller is caught.

## The C6 `all` part on the VM

Deployed: legion `1d024227`, agent-infra `ddb468fd`. Configuration parts `['all', 'slice']`. Every run used `--jobs 48`. The C-facts cache was warm in every run: a hard-linked copy of the live entries, and each run parsed 0 of 432 files. The runs ran one after another, never at the same time.

| Run | Code | Image cache | Wall time (`/usr/bin/time -v`) | Peak RSS | Exit |
| --- | --- | --- | --- | --- | --- |
| before, warm | `1d024227` (deployed) | warm, old key | 832.5 s (13:52.49) | 11.69 GB | 0 |
| after, cold image | this unit | empty | 589.9 s (9:49.93) | 11.67 GB | 0 |
| after, warm | this unit | warm | **575.9 s** (9:35.90) | 11.69 GB | 0 |
| after, warm, cProfile | this unit | warm | 1,289.1 s (21:29.11) | 11.69 GB | 0 |
| corrupt | this unit | every facts entry `{` | 26.3 s | 1.05 GB | **3** |

All eight outputs (`build.json`, `checks.json`, `exclusions.json`, `graph.json`, `measures.json`, `owners.json`, `report.md`, `topics.json`) have equal SHA-256 in five builds: the parent's `out/all-profile` (old code), the unprofiled before-warm run, after-cold, after-warm-profile and after-warm. For example, `graph.json` is `07667e68…` and `checks.json` is `2f702316…` in all five.

C2 on real data: with every facts entry of a copied cache overwritten by `{`, the build printed `cgraph: image cache entry …/cache-corrupt/image/9d554fc3….json is malformed; remove the cache directory …/cache-corrupt/image`. It exited 3, and `sha256sum -c` of the eight earlier outputs gave OK for each.

### Profile split, before and after (cumulative seconds under cProfile)

Before: the parent's `/theorem/linux-graph/profile-all.pstats`. After: `profile-all-after.pstats` (path below).

| Bucket | Before | After |
| --- | --- | --- |
| whole run (`pipeline.run`) | 1,550.1 | 1,286.4 |
| C-facts cache lookup (432 units) | 50.7 | 50.6 |
| image: `Image.__init__` | 5.0 | 5.0 |
| image: `Image.facts` (276,573 calls) | 268.2 | **2.3** |
| …of which `ImageCache.key` (8,880 calls) | 265.2 | 0.3 (`symbol_key` + `facts_key`) |
| link (`link.build`, image facts included) | 364.6 | 101.6 |
| checks (`calls`, `accesses`, `structure`, `hard`) | 4.8 | 6.0 |
| owners (`owners.build`) | 1.0 | 1.0 |
| measures (`measures.build`) | 1,108.9 | 1,106.0 |
| …of which `measures._interfaces` | 1,098.1 | 1,095.0 (not changed by this unit) |

### The 436 s target

The warm `all` build takes 575.9 s, 139.9 s over 436 s. The image layer is now 2.3 s of a 1,286 s profile. No image-cache change can close the gap, so this unit makes none.

The measured cause is in `measures.py:_interfaces` (U3's file). Its generator at `measures.py:200` calls `re.search` with a pattern built from each node name. In the after profile that generator takes 1,087.0 s cumulative over 11,021,499 calls. `re._compile` is called 5,727,917 times and takes 926.4 s. The before profile is nearly the same: 1,090.0 s, with 5,707,659 actual compiles taking 928.3 s. `re._MAXCACHE` is 512 on this interpreter, and the `all` graph has 11,538 distinct node names. So the compiled-pattern cache cannot hold the patterns, and almost every search compiles its pattern again.

Under cProfile, `measures.build` is 86 % of the after run. Assuming that share holds without the profiler, it accounts for about 495 s of the 575.9 s wall time. [INFERENCE: profiler overhead is not uniform. Regular-expression compilation is pure Python and is inflated more than C code. The unprofiled share is therefore not measured.]

This report does not optimize that file, because the unit boundary excludes `link.py` and `measures.py`. The coordinator owns that decision.

## Checks

The exact cgraph CI lane ran on the VM with the pinned `uv 0.12.5`, on the final tree in `/theorem/linux-graph/cgraph-image-task20-final`:

```sh
uv sync --frozen
uv run ruff check src tests          # All checks passed!
uv run ruff format --check src tests # 29 files already formatted
uv run mypy                          # Success: no issues found in 19 source files
uv run pytest -q                     # 200 passed in 4.14s
```

`bash .github/scripts/check-private-names.sh`, run on the same tree after `git init && git add -A`, printed: `no file or path names the private deployment repository or the company, and no text names a private internal host or a Tailscale MagicDNS host (4829 files in the repository)`.

## Hardening ledger

No shortcut. No test was weakened, skipped or deleted. The two existing image-cache tests were rewritten to the new interface and strengthened: the warm-run proof is now a disassembler that raises when used, in place of a patched `decode`. The old `image.py` and `imagecache.py` code they targeted no longer exists.

## Writes outside the local workspace

All writes were on `linux28-poc-build` under `/theorem/linux-graph/`, in disposable task directories:

- `cgraph-image-task20`: a source copy, a virtual environment, the `uv` binary, and the profile and baseline runs. Its caches hard-link the live C-facts entries, which are immutable and never written through those links.
- `cgraph-image-task20-mutation`: the mutation checks.
- `cgraph-image-task20-final`: the final lane.

After the runs, all three were removed. The evidence was first moved to `/theorem/linux-graph/cgraph-image-task20-evidence/`: `profile-all-after.pstats`, the `/usr/bin/time -v` logs (`cold.log`, `warm-profile.log`, `warm.log`, `corrupt.log`, `before-warm.log`), `status`, `out-corrupt.before.sha256`, `out-corrupt.check` and `before-DEPLOYED`. The live `/theorem/linux-graph/cache` is unchanged: `facts/` is 999 MB in 4,767 entries. Nothing under `out/` or the deployed checkout was written.
