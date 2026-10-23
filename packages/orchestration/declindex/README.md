# The declaration index and the similarity gate

`cmd/declindex` answers questions about a built Lean project and protects the concept registry that its owners share. The command's help is the short command reference; this document describes the persisted data and the landing contracts. For container acceptance drivers, see [the end-to-end driver](e2e/README.md).

## Index data and storage

The index has two parts in one SQLite file next to the build:

- The `.ilean` part records declarations, source positions, direct uses, and declaration edges. It also records `sources`: every indexed Lean source path and the Git blob ID of the bytes that the index read. A changed source can therefore be distinguished from a source that the index already read.
- The compiled part reads `.olean` files with an exporter built by the tree's Lean toolchain. It records each constant's printed type, compiled statement hash, relation atoms, and the registry facts that its statement states.

The index metadata has `commit`, the Git commit it read, and `prev_commit`, the commit it replaced during the latest refresh. A post-landing registry check compares those two commits. A module whose source is newer than its build, or whose recorded declaration position no longer holds its name, is unreadable until a build replaces the output; affected queries exit 3.

The declaration graph is intentionally not in the Postgres ledger. SQLite stores the declaration, use, edge, compiled-constant, atom, fact, source, and metadata tables, with the reach graph in a sibling file. Postgres stores contract owners, statement hashes, versions, and change-lock reads. This keeps index queries local to the build and keeps ledger transactions limited to the data that coordinates landings.

## Concept registry

The registry is format 2 and is rooted at the project tree's configured `registry` directory:

```text
registry/
├── registry.yaml
└── concepts/
    └── <key>.yaml
```

`registry.yaml` contains `version: 2` and optional imports and opens for the forms module. Each concept file has one name, home file, one-sentence meaning, optional `extends` and forms, and, for gate additions, the proposal ID, proposer, compiled hash, accepted-by decision, accepted near matches, and stored source. A concept with a compiled hash is keyed as `concepts/<compiled_hash>.yaml`; a librarian-written concept without one is keyed as `concepts/<name>.yaml`.

One concept per file lets an owner open a change lock on only the proposed addition. The gate writes no registry file. The owner writes the returned `addition.content` to `addition.path`, opens or extends its migration with `ledger open --write <addition.path>`, and lands it normally.

The loader reports duplicate names, duplicate compiled hashes, wrongly keyed files, and missing `extends` targets as registry defects. `refresh` and `gate` report those defects as notes and continue. `ledger` reports them in its registry result and exits 1. A format-1 registry is not accepted.

Stored source is untrusted input. Refresh and the ordinary registry forms module never compile it. Only the gate and the post-landing recheck compile stored source, and both do so in the sandbox.

## Similarity gate and librarian route

An owner runs:

```text
declindex --config <cfg> gate --proposer <owner-id> [--accepted-by <owner-id>] PROPOSAL.json
```

`PROPOSAL.json` has `id`, `concept` (`name`, `home`, `meaning`, and optional `extends`), and `source`, a Lean module that declares the concept. It carries no forms. Before deciding, the gate requires the sandbox, an index with a compiled part at the tree's HEAD, and a clean registry directory. Failure of any of those checks is exit 3 with a cause on stderr and no librarian request.

| Exit | Verdict | Owner action |
| --- | --- | --- |
| 0 | `added`: no registered fact or existing declaration states the proposal. | Write and land the returned addition. |
| 1 | `refused`: the proposal is an exact duplicate. | Use the named existing concept. |
| 3 | Unable to check. | Keep the item open and retry after the cause is repaired or the index is refreshed. |
| 4 | `near`: a registered fact is restated. | Wait for the librarian's decision. |
| 5 | `name-taken`: a concept or declaration already has the proposal name. | Use that concept, or use another name for a different statement. |
| 2 | Usage error. | Correct the command invocation. |

The comparison has no embedding threshold and no partial-atom threshold. Exact compiled hashes are refused. A statement that directly states a registered form, or states it through a declaration in the proposal, is near. A proposal that the gate cannot compare is also near rather than accepted. `--accepted-by` records a librarian decision that a near proposal is new; the resulting addition records `accepted_by` and `accepted_near`.

For every near result, the configured `librarian: [cmd, args…]` receives:

```text
cmd args… --id <verdict-id> --proposer <owner-id> --proposal <file>
```

The file is mode 0600 in the gate work directory and is removed after the command exits. It contains fixed text, the deterministic ID and validated proposer outside a nonce-delimited block, and one JSON line inside the block. The JSON carries the verdict and the untrusted proposal. The command receives empty standard input. Exit 0 means delivery; another exit or a 120-second timeout makes the gate exit 3. The post-landing recheck uses the same command with `--id recheck-<added-hash>-<other-key>` and `kind: "recheck"` in the envelope.

### Untrusted source confinement

The gate and recheck apply three controls before accepting a stored or proposed source:

1. A text filter rejects obvious executable forms.
2. Lean's parser allow-list admits declarations and limited surrounding syntax, and refuses forms that execute during elaboration.
3. `confine` runs checking, compilation, and export in a fresh Landlock and seccomp sandbox. It requires Landlock ABI 3 or later, allows writes only to fresh scratch directories, permits only the launched process, and blocks later `execve`, forks, sockets, mounts, namespace operations, and other privileged interfaces.

Without that sandbox, the source is not compiled and the relevant operation exits 3. The detailed container controls and invocation are documented in [the end-to-end driver](e2e/README.md).

## Landing interfaces

### `refresh`

The landing hook invokes:

```text
declindex refresh --config <cfg> && declindex ledger --config <cfg>
```

The landing program sets `LAND_COMMIT` and `LAND_PART`; `declindex` reads `LAND_COMMIT` and does not read `LAND_PART`. With a 40-hex `LAND_COMMIT`, `refresh` runs under the index writer lock. It refuses tracked tree changes, recognizes an index that already covers the commit, fetches a missing commit from configured `remote`, checks out the commit, runs configured `build` within `build_timeout` (six hours by default), records `sources`, and stores the replaced commit as `prev_commit`.

`refresh` exits 0 after reading the build, 3 when it cannot safely read it, 75 for a failed fetch, build timeout, or signalled build, and 2 for usage. A configured build that exits nonzero exits 3.

### `land-hash`

The landing program calls `declindex land-hash` twice for every registered contract:

1. At the base tree, with the index at that tree. Unchanged declarations are answered from the index; no candidate build directory is needed.
2. At the built candidate tree. For each source blob that differs from the indexed `sources` blob, `LAND_BUILD_DIR` must hold a fresh Lake `.ilean` and trace for that tree. The candidate answer gives the changed contract's candidate hash and path; unchanged declarations keep the indexed answer.

The command takes no positional arguments. Its environment is:

- `LAND_TREE`: the 40-hex Git object whose tree is being answered.
- `LAND_GIT_DIR`: a Git directory that can read `LAND_TREE` and its blobs.
- `LAND_NAMES` or `LAND_NAMES_FILE`: a JSON array of distinct, non-empty, fully qualified names. The file form is required for large requests. If both are set, their arrays must be equal.
- `LAND_BUILD_DIR`: the candidate build root when a requested declaration is in a changed source file.

On exit 0, each requested name produces one line in request order:

```text
name<TAB>hash<TAB>path
```

The `hash` is the 16-hex text statement hash and `path` is the repository-relative declaration path. An absent declaration is `name<TAB>absent<TAB>-`; an ambiguous declaration is `name<TAB>ambiguous<TAB><sorted,comma-joined paths>`. Exit 3 writes no stdout when the request, Git tree, source snapshot, candidate build, trace, or parsed declaration is unusable. Exit 2 is a usage error.

### `ledger`

`ledger` checks the ledger only after the index is at the tree's HEAD. It opens the ledger through `ledger.OpenExisting`: a database without every required schema version, or without the configured repository, is not changed and is unable to check. The DSN comes from `LEDGER_DSN` or `--dsn-file`. The landing adapter can instead use `--contracts-file FILE --emit FILE`; that mode emits required registrations and never connects to Postgres.

The contract check registers selected, owned declarations that the ledger lacks, then compares every ledger contract whose name has no `:` with the index by exact name across every module. It reports a changed hash as a defect, no declaration as gone, and multiple declarations as ambiguous. Names with `:` are non-Lean ledger contracts and are not compared with declarations.

The registry check loads format 2, lists the concept files added in `prev_commit..commit`, and recompiles each added source only in the sandbox. It records additions, loader and recheck defects, near pairs, and an inability reason in the `registry` JSON member. Every near pair runs the librarian command once unless its accepted-near record suppresses it.

The JSON output is:

```json
{"commit":"…","contracts":{…},"registry":{"added":[],"defects":[],"near":[],"unable":""}}
```

The final stderr line counts contracts, registrations, contract defects, gone, unowned, ambiguous, and registry additions, near pairs, and defects. `ledger` exits 0 only when both checks are clean; 1 for a contract hash defect, gone, unowned, ambiguous contract, registry defect, or added near pair; 3 when it cannot check; 75 for a ledger connection failure; and 2 for usage. If a recheck is unable but either check found a defect, it exits 1 and includes the inability in the report.

## Baseline measurements and acceptance records

The following baseline measurements describe the indexed project, not acceptance evidence for this change: 52,529 modules, 545,934 declarations, 9,686,627 uses, 4,389,369 edges, and 789,877 compiled constants. The initial refresh took 15 minutes 43 seconds; an unchanged update took 2.2 seconds; a one-module compiled update took 3.4 seconds. Process-inclusive query times were 0.014 seconds for `where`, 0.2 seconds median and 0.67 seconds worst for `users`, 0.34 seconds for `states`, 0.11 seconds for `same`, and about 5 seconds for `gate`.

No E1–E5 acceptance run record exists yet. Do not treat the baseline numbers as their result. Before this document is accepted, its acceptance records must be amended with each concrete run ID, command output, revision, binary hash, duration, and measured outcome at these paths:

| Acceptance row | Required record path |
| --- | --- |
| E1 parity | `/theorem/infra145/runs/infra145-e1-<UTC yyyymmddThhmmZ>/` |
| E2 recall and exact duplicates | `/theorem/infra145/runs/infra145-e2-<UTC yyyymmddThhmmZ>/` |
| E3 gate and librarian route | `/theorem/infra145/runs/infra145-e3-<UTC yyyymmddThhmmZ>/` |
| E4 sandbox controls | `~/.cache/infra109/infra145-runs/infra145-e4-<UTC yyyymmddThhmmZ>/` |
| E5 `land-hash` | `/theorem/infra145/runs/infra145-e5-<UTC yyyymmddThhmmZ>/` |

The required records are generated by the end-to-end drivers. They are the evidence for the baseline-scale claims and the acceptance outcomes, not this README.
