DONE

# INFRA-143 U3 performance: warm `all` below 436 s

Base: `336a2ba0` (`perf(cgraph): key image facts on what decoding reads`), which carries U1, U2, U3 and U4.
Branch `cgraph-link`. One commit; not pushed.

## Result on `linux28-poc-build` (part `all`, `--jobs 48`, 432 units, every unit's facts from the cache)

| Run | Code | Wall time | Peak RSS |
| --- | --- | --- | --- |
| `base-cold` | `336a2ba0` | 592.8 s | 11.1 GB |
| `base-warm` | `336a2ba0` | 571.4 s | 11.1 GB |
| `fw1`: first run, empty link and interface caches | this commit | 136.6 s | 11.1 GB |
| `fw2`: unchanged rebuild, link core reused | this commit | **45.9 s** | 2.5 GB |
| `fcwarm`: only the contract graph changed | this commit | **45.0 s** | 2.5 GB |
| `base-changed`: the same contract change | `336a2ba0` | 619.8 s | 11.1 GB |

The 436 s target (198 landings a day) holds on every path: with the core cache empty the run takes 136.6 s.

## Profiles (cProfile, VM)

Baseline: `/theorem/linux-graph/out/all-u3-profile-baseline.pstats` (1560 s under the profiler, code `1d024227`):

- `measures._interfaces`: 1088.5 s. 5.73 million `re.search` calls, one per (interface, C name), each
  compiling its own pattern because 11,538 names overflow `re`'s 512-entry cache: 919.8 s in `re._compile`.
- `link.build`: 379.4 s, 364.4 s of it in `Linker.effective` (folding).
- `cache.lookup` (the facts cache key over 432 units and 1.65 million include probes): 51.0 s.
- Everything else below 6 s each.

After: `/theorem/linux-graph/cgraph-u3-final/fw3.pstats` (97.6 s under the profiler, warm):

- `cache.lookup`: 51.3 s (U1's facts-cache key; unchanged by this commit).
- `measures.build`: 14.6 s, of which `_interfaces` is 2 s.
- `link.build`: 10.2 s: 5.8 s re-reading every part function's image facts to check the core, 2.5 s
  the contract layer, 1.6 s loading the core.
- The image open, output rendering and hard checks: 13.6 s together.

## Byte equality (SHA-256 of every output but `build.json`)

All seven outputs (`graph.json`, `checks.json`, `exclusions.json`, `owners.json`, `topics.json`,
`measures.json`, `report.md`) are equal in each pairing:

- `base-warm` = `fw1` = `fw2` = `fw3`; for example `graph.json` `07667e68…`, `measures.json` `c927a17f…`.
- The contract change (one function's contract renamed, one table edge dropped):
  `fcwarm` (core reused) = `new-cfull` (core built from nothing) = `base-changed` (the `336a2ba0` code);
  `graph.json` `6be09268…`, `measures.json` `e2f7e947…`. It differs from the unchanged build, so it
  changes the graph: 84,859 edges against 84,860.
- On the fixture, in a container, for parts `store`, `both` and `flow`: `1d024227` code, this code
  without a cache, with a cold cache and with a warm cache give equal bytes for every output.

Records: `/theorem/linux-graph/cgraph-u3-final/status` and `status2`, one line per run and per file.

## What changed

- `measures.py` (`_interfaces`):
  - Each name made only of identifier characters is a dictionary key (name → groups). A declaration
    is split once into identifier runs, and a name matches where a run equals it. That is the old
    whole-name rule, `(?<![A-Za-z0-9_])name(?![A-Za-z0-9_])`.
  - A name with other characters (`next_id.counter`) keeps one compiled pattern, tried only where all
    of its runs occur. A differential test checks the index against the old rule for every name shape.
  - Each declaring module is read once per run. Its top-level lines and per-line runs are cached at
    `<cache>/interfaces/<sha256>.json`, keyed by the module's contents and the parse rules, so only
    a changed module is parsed again.
  - A declaration that cannot be read is now `InputError` (exit 3). Before, it silently matched no
    group. The VM's 2,412 interfaces in 1,443 modules all resolve (checked before the change).
- `link.py`:
  - `build` splits the link. The core is everything the unit facts, the image and the configuration
    decide. It is kept at `<cache>/link/<sha256 of the part's files>/<key>.json`, one per part, and a
    new core removes the part's earlier ones.
  - The contract layer runs on every build: each function's `contract` and its provenance entry, the
    callback-table edges and their C3 exclusions, `expected_functions`, contract functions without C,
    and `contract_mapping_duplicates`.
  - The core key covers:
    - every package source file, the Python version, and the capstone and pyelftools versions;
    - the full vocabulary and `[image]` rules;
    - the part's files;
    - each unit's facts-cache key;
    - the SHA-256 of the image and of `System.map`;
    - the image copy each definition's contract address pins.
  - A loaded core is used only while the SHA-256 of the image facts of its part functions, read
    through U4's image cache, equals the one it was built from. So U4's exit-3 refusal of a malformed
    image-cache entry still holds with a warm core.
  - A malformed core is `CacheError` (exit 3), and the message names the directory to remove.
- `pipeline.py`: it passes `link.LinkCache(cache_directory, {facts keys, image, system_map})` to
  `link.build` and the cache directory to `measures.build`. This is the minimal hook.
- `README.md`: the two new cache layers.

## Tests (test first; each red run recorded before the code)

- `tests/test_measures.py` (new):
  - names are matched from the index, with one compile for the dotted name and no `re.search` per name;
  - each module is read once;
  - only a changed module is parsed again;
  - a change to the parse rules parses every module again;
  - a malformed entry is `CacheError`;
  - a missing module is `InputError`;
  - the differential index-against-regex test.
- `tests/test_link_cache.py` (new):
  - a contract-only change reuses the core, and its bytes equal a link without a cache;
  - an unchanged rebuild reuses the core byte for byte;
  - a change to the C, the configuration, `System.map` or a contract pin computes the core again;
  - one core per part;
  - a malformed core is `CacheError`;
  - changed image facts compute the core again.
- Red runs:
  - the first `_interfaces` tests failed on the old signature;
  - the link tests failed on the missing `_link_core` and on no `link/` entry;
  - the parse-rules test failed: 0 parses, expected 2;
  - the image-digest test failed, together with U4's four `test_corrupt_image_cache_entry_*` cases
    (`exit 0 != 3`), before the image-facts digest was added.

## Checks

- VM lane (`linux28-poc-build`, `/theorem/linux-graph/cgraph-u3-final/final/cgraph`, this commit's
  package on `336a2ba0`): `uv sync --frozen`, `ruff check src tests`, `ruff format --check src tests`,
  `mypy` and `pytest -q` all exit 0. 215 passed.
- Container lane (`ghcr.io/astral-sh/uv:python3.13-bookworm-slim`), the same five steps: all passed,
  215 passed.
- `bash .github/scripts/check-private-names.sh`: no findings. `git diff --check`: clean.

## Seams and notes for the parent

- Disk: a core for `all` is 158.7 MB, and one is kept per part.
- The remaining 51 s is U1's `cache.lookup`: re-hashing the include probes of 432 units. It is outside U3.
- The coordinator's next directive, the `contract` field as a list for a header helper, is a separate
  follow-up commit.
- Writes outside this workspace: only the VM scratch trees `/theorem/linux-graph/cgraph-u3-perf`,
  `/theorem/linux-graph/cgraph-u3-final`, `/theorem/linux-graph/out/all-u3-profile-baseline*` and
  `/theorem/linux-graph/out/all-u3-baseline-plain*`. The live cache, the outputs and the deployment were
  not modified; every run used a copy of the cache.
- Hardening ledger: none.
