# cgraph

A Python package (uv, Python 3.12) that builds the graph of a C program and its RISC-V image. `README.md`
is the user's reference: the configuration, the outputs, the exit status.

## Commands

```bash
uv sync --frozen                 # the locked environment
uv run ruff check src tests      # lint
uv run ruff format src tests     # format
uv run mypy                      # strict type check of src/
uv run pytest -q                 # tests on tests/fixtures/mini
```

CI runs the same steps in the `cgraph` job of `.github/workflows/pr-and-main.yaml`.

## Layout

| File | What it does |
| --- | --- |
| `src/cgraph/config.py` | The TOML configuration. Every program-specific word lives there, never in code. |
| `src/cgraph/commands.py` | Parser arguments from a compilation database or a flag template. |
| `src/cgraph/clangfacts.py`, `libclang.py` | One translation unit to JSON facts; the libclang C calls the binding lacks. |
| `src/cgraph/cache.py` | Per-unit facts keyed by the hash of the code, the vocabulary, the arguments and every included file. |
| `src/cgraph/image.py` | ELF symbols, System.map and capstone: calls, data addresses, offsets. |
| `src/cgraph/imagecache.py` | Per-function image facts keyed by the image inputs, the symbol, and the only C facts decoding reads (own result registers, direct callees' parameter counts). |
| `src/cgraph/contracts.py` | The contract graph (functions, callback tables, interfaces). |
| `src/cgraph/link.py`, `structure.py` | Folding, ids, nodes and edges; structure links and subsystems. |
| `src/cgraph/checks.py`, `owners.py`, `measures.py`, `report.py` | Layer and hard checks, owners and topics, numbers, the Markdown report. |
| `src/cgraph/pipeline.py`, `cli.py` | One build, and the `cgraph` command. |

## Rules

- The link is a pure function of the unit facts, the image and the contract graph. Anything that reads
  a C file must happen in `clangfacts.py`, so the cache key covers it and an incremental build equals a
  full one (`tests/test_build.py`).
- Outputs are byte-stable: sorted lists, sorted keys, paths relative to the C tree, inputs named by SHA-256.
- A change to the fixture's C or assembly needs its image rebuilt with `tests/fixtures/mini/build.sh`
  (clang and ld.lld that target RISC-V) and both committed together.
- This repository is public: tests and examples use the fixture program, never a real program's names.
