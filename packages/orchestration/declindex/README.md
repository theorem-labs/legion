# The declaration index and the similarity gate

`cmd/declindex` answers questions about a built Lean project and protects the concept registry that its owners share. The command's help is the short command reference; this document describes the persisted data and the landing contracts. The container acceptance drivers are in [e2e/README.md](e2e/README.md).

## Index data and storage

The index has two parts in one SQLite file next to the build:

- The `.ilean` part records declarations, source positions, direct uses, and declaration edges. It also records `sources`: every indexed Lean source path and the Git blob ID of the bytes that the index read. A changed source can therefore be distinguished from a source that the index already read.
- The compiled part reads `.olean` files with an exporter built by the tree's Lean toolchain. It records each constant's printed type, compiled statement hash, relation atoms, and the registry facts that its statement states.

The index metadata has `commit`, the Git commit it read, and `compiled_commit`, the commit whose compiled part it holds; the gate and the post-landing recheck refuse an index whose two differ. The `landings` table records each refresh that moved the index to another commit: that commit and the commit it replaced. The post-landing registry check of a landing compares those two commits, however many landings came after it. A module whose source is newer than its build, or whose recorded declaration position no longer holds its name, is unreadable until a build replaces the output; affected queries exit 3. A tree that is not a Git work tree has no commit: `refresh` reads it and records no commit, the queries and `status` print a note, and `gate` and `ledger`, which compare the index's commit with the tree's, exit 3.

The index records its schema version, which changes whenever what an index holds changes. After an upgrade to a `declindex` of another schema version, the next `refresh` rebuilds the index from nothing; until it has, `gate`, `ledger` and `land-hash` refuse the index with exit 3 and name that refresh.

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

A concept with forms and no stored source is matched through its forms, whether or not the index declares its name. A concept with no form is matched through its own declaration's body, so it is matched only once the index declares it. A gate addition that has not landed is compiled from its stored source by the gate alone.

One concept per file lets an owner open a change lock on only the proposed addition. The gate writes no registry file. `addition.path` is relative to the repository root and slash-separated, for example `registry/concepts/<compiled_hash>.yaml`, so it names the same file in every worktree. When the configured tree is a directory below the repository's root, the path starts with that directory, as `refresh` recorded it in the index (for example `proj/registry/concepts/<compiled_hash>.yaml`). The owner writes the returned `addition.content` to that path in its own worktree, opens or extends its migration with `ledger open --write <addition.path>`, and lands it normally.

The loader reports these registry defects: a concept file it refuses (YAML that does not decode, an unknown key, a concept that fails its checks, such as a two-sentence meaning), which it leaves out; duplicate names; duplicate compiled hashes; wrongly keyed files; and missing `extends` targets. The gate also reports a pending concept whose stored source the text filter or Lean refuses, and leaves it out of the forms. A defect is one concept's: `refresh` and `gate` report defects as notes and continue, and `ledger` reports them in its registry result and exits 1. Only a missing, malformed or format-1 `registry.yaml` stops them.

Stored source is untrusted input. Refresh and the ordinary registry forms module never compile it. Only the gate and the post-landing recheck compile stored source, and both do so in the sandbox.

## Similarity gate and librarian route

An owner runs:

```text
declindex --config <cfg> gate --proposer <owner-id> [--accepted-by <owner-id>] PROPOSAL.json
```

`PROPOSAL.json` has `id`, `concept` (`name`, `home`, `meaning`, and optional `extends`), and `source`, a Lean module that declares the concept. It carries no forms, and any `accepted_by` or `accepted_near` it carries is ignored. Before deciding, the gate requires the sandbox, an index at the tree's HEAD whose compiled part is of that commit, and a registry directory without local changes once it has taken its snapshot of the registry at that commit. Failure of any of those checks is exit 3 with a cause on stderr and no librarian request. The verdict's `defects` list the registry defects that the decision left out; the gate also prints them as notes.

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

The file is mode 0600 in the gate's temporary work directory and is removed after the command exits. It contains fixed text, the deterministic ID and validated proposer outside a nonce-delimited block, and one JSON line inside the block. The JSON carries the verdict and the untrusted proposal. The command receives empty standard input. Exit 0 means delivery; another exit or a 120-second timeout makes the gate exit 3. The post-landing recheck sends the same envelope with `--id recheck-<added hash>-<other>` and `kind: "recheck"`; see `ledger` for who delivers it. `<added hash>` is the added concept's compiled hash. `<other>` is the other concept's compiled hash, or, for a librarian-written concept without one, `name.` followed by the hexadecimal of the name's UTF-8 bytes. So every recheck ID matches `^recheck-[A-Za-z0-9._:@/-]+$`, whatever characters a Lean name holds, and two pairs never share an ID.

### Untrusted source confinement

The gate applies three controls before it accepts a proposed source or a pending concept's stored source; the post-landing recheck applies the last two to an added concept's stored source, which has landed:

1. A text filter rejects obvious executable forms.
2. Lean's parser allow-list admits declarations and limited surrounding syntax, and refuses forms that execute during elaboration.
3. `confine` runs checking, compilation, and export in a fresh Landlock and seccomp sandbox. It requires Landlock ABI 3 or later, allows writes only to fresh scratch directories, permits only the launched process, and blocks later `execve`, forks, sockets, mounts, namespace operations, and other privileged interfaces.

Without that sandbox, the source is not compiled and the gate exits 3 (the recheck, 75). The gate checks for it twice. First, before it looks up any toolchain or builds the exporter, it checks without starting a process that the kernel and architecture can build the sandbox. Then, before it compiles any source, it runs the toolchain's `lean --version` in the sandbox; that second check is the authoritative one, and the recheck runs it too. The sandbox controls are tested in containers by `e2e/sandbox-e2e.sh` ([e2e/README.md](e2e/README.md)), and by the gate's Lean tests where `DECLINDEX_SANDBOX_TEST=1` (a disposable container or CI runner).

## Landing interfaces

### Landing containers

The landing program's container contract decides each container's mounts and network. Besides `TMPDIR`, where `declindex` makes its registry snapshots, its proposal work and the sandboxed and toolchain runs' scratch directories, it needs these writable places and nothing else:

| Command | Writes |
| --- | --- |
| `refresh` with `LAND_COMMIT` | The tree (fetch from `remote`, checkout, `build`) and the index directory. |
| `ledger` | The `--emit` file. The index directory can be read-only. |
| `gate` | The index directory only to build the exporter, when no refresh with `lean.export` built it there first. |
| `land-hash` | Nothing. It reads the index, `LAND_GIT_DIR` and `LAND_BUILD_DIR`; the index directory can be read-only. |

In a read-only index directory, `ledger`, `land-hash` and `gate` read the database file alone and hold, shared, the index's locks for what they read. A refresh holds its refresh lock (`<db>.lock`) across the fetch, checkout, build and its reads of the tree, and holds the write lock (`<db>.write.lock`) only while it writes the index; each write ends with a checkpoint of the write-ahead log into the database file. `land-hash` reads only the index, so it holds only the write lock: while a refresh fetches, checks out and builds, `land-hash` answers from the index of the refresh before, and it waits only while the refresh writes. `ledger` and `gate` also read the tree (its HEAD, the registry's commits, and the build that they compile against), which a refresh changes, so they also hold the refresh lock. They wait for a running refresh, including its build (at most `build_timeout`), and a refresh waits for them. The recheck in `ledger` uses the exporter that `refresh` with `lean.export` built in the index directory.

The configuration's absolute paths, such as the toolchain, `lean.cc`, `remote` and the owner map, must exist at the same paths inside the container. A container image with the pinned toolchain copies it from a worker image pinned by digest, as `e2e/Dockerfile` does (the required `LEAN_IMAGE` build argument; [e2e/README.md](e2e/README.md)).

Deployment requirement: the identity that pulls the worker image on the landing machine needs read access to the registry that holds it; without it the pull fails with HTTP 403. Grant that access, or copy the image by digest to a registry that identity can read, before the landing program builds these containers.

### `refresh`

The landing hook invokes:

```text
declindex refresh --config <cfg> && declindex ledger --config <cfg>
```

The landing program sets `LAND_COMMIT` and `LAND_PART`; `declindex` reads `LAND_COMMIT` and does not read `LAND_PART`. With a 40-hex lowercase `LAND_COMMIT`, `refresh` runs under the index's refresh lock, and takes the write lock only to write the index ([Landing containers](#landing-containers)). It refuses tracked tree changes, recognizes an index that already covers the commit, fetches a missing commit from configured `remote`, checks out the commit, runs configured `build` within `build_timeout` (six hours by default), records `sources`, reads the reference files, and records the landing (the commit and the one it replaced). With `lean.export` it then reads the compiled part under the same refresh lock. An index covers the commit only when it read that commit or a descendant completely: without unreadable modules, with its declaration graph, and with the compiled part when `lean.export` is on. So a retry after a failed build, reference read or compiled step runs again. A refresh queued behind another's lock ends when it gets SIGTERM, SIGINT or SIGHUP.

`refresh` exits 0 after reading the build, 3 when it cannot safely read it, 75 for a failed fetch, build timeout, or signalled build, and 2 for usage. A configured build that exits nonzero exits 3. A registry defect is a note, not a failure.

### `land-hash`

The landing program calls `declindex land-hash` twice for every registered contract:

1. At the base tree, with the index at that tree. Unchanged declarations are answered from the index; no candidate build directory is needed.
2. At the built candidate tree. For each source blob that differs from the indexed `sources` blob, `LAND_BUILD_DIR` must hold a fresh Lake `.ilean` and trace for that tree. The candidate answer gives the changed contract's candidate hash and path; unchanged declarations keep the indexed answer.

A declaration is answered from the index only when `LAND_TREE` has its source file with the same blob as the indexed `sources` row. A module that the index read without a `sources` row, such as an ignored source file, is not answered from the index.

The configured tree can be a directory below its repository's root. The index keys `sources` and declaration paths relative to the tree, and `refresh` records the tree's directory in the repository (`git rev-parse --show-prefix`). `land-hash` reads that directory from the index, never the configured tree: it lists the sources of `LAND_TREE` below that directory, compares them with the index by their tree-relative paths, and prints each `path` with the directory in front. For a tree at the repository's root the directory is empty.

A changed module's build output is fresh only when all of these hold:

- `LAND_BUILD_DIR` holds that module's source with exactly the candidate blob's bytes.
- Its `.ilean` is the output its Lake trace records: Lake's hash of the file equals the trace's `outputs.i`.
- A build trace's source input hash equals the blob's. A fetch trace, which Lake writes when its artifact cache serves the module, has no source input. It binds the outputs only, and the source only by being newer than it. A cache hit is still a legitimate result, because Lake serves one only when the key it computed from the module's inputs in that build matches.

So for a fetch trace, nothing in the build directory ties the outputs to the candidate's source. What keeps another build's `.ilean`, copied in together with its matching fetch trace, out of the answer is the landing program's contract: it builds the candidate in a fresh container that no agent can write. Any other output that fails these checks exits 3.

The command takes no positional arguments. Its environment is:

- `LAND_TREE`: the 40-hex Git object whose tree is being answered. Its tree is the repository's root tree, also when the configured tree is a directory below it.
- `LAND_GIT_DIR`: a Git directory that can read `LAND_TREE` and its blobs.
- `LAND_NAMES` or `LAND_NAMES_FILE`: a JSON array of distinct, non-empty, fully qualified names. A top-level declaration's full name has no dot, so a name need not contain one. The file form is required for large requests. If both are set, their arrays must be equal.
- `LAND_BUILD_DIR`: the candidate build root when a requested declaration is in a changed source file. It is the candidate's copy of the configured tree, so its sources are at their tree-relative paths.

On exit 0, each requested name produces one line in request order:

```text
name<TAB>hash<TAB>path
```

The `hash` is the 16-hex text statement hash and `path` is the repository-relative declaration path. An absent declaration is `name<TAB>absent<TAB>-`; an ambiguous declaration is `name<TAB>ambiguous<TAB><sorted,comma-joined paths>`. Exit 3 writes no stdout when the request, Git tree, source snapshot, candidate build, trace, or parsed declaration is unusable, or when a path that an answer line prints contains a tab, carriage return, newline, or comma. Such a character in a source path that no answer prints does not affect the call. Exit 2 is a usage error.

### `ledger`

`ledger` checks the ledger only after the index is at the tree's HEAD. It opens the index read-only. It opens the ledger through `ledger.OpenExisting`: a database without every required schema version, or without the configured repository, is not changed and is unable to check. The DSN comes from `LEDGER_DSN` or `--dsn-file`, for an operator's check. The landing hook instead uses `--contracts-file FILE --emit FILE`; that mode never connects to Postgres or runs the librarian command, and `--dsn-file` with it is a usage error.

The contract check registers selected, owned declarations that the ledger lacks, then compares every ledger contract whose name has no `:` with the index by exact name across every module. It reports a changed hash as a defect, no declaration as gone, and multiple declarations as ambiguous. Names with `:` are non-Lean ledger contracts and are not compared with declarations.

The registry check reads the registry as committed at the index's commit, never the working tree, so a local edit after `refresh` does not change what it audits. It checks one landing: `LAND_COMMIT`, or the index's commit without it. It lists the files directly under `concepts/` with a `.yaml` extension (the files the registry loader reads) that the commits from the commit that landing's refresh replaced to that landing's commit added, with rename detection off, so a file added next to a deleted, similar one is an addition. When no refresh stopped at `LAND_COMMIT` (a later landing's refresh read it), the landing that read it rechecks its additions. Each added concept that the loader read is recompiled from its stored source only in the sandbox. The check records additions by their paths from the repository's root (the paths the gate printed as `addition.path`, also for a tree below that root), loader and recheck defects (a stored source that Lean refuses or does not compile is one), near pairs, and an inability reason in the `registry` JSON member. Each near pair not suppressed by an accepted-near record goes to the librarian once:

- With `--emit`, the hook has no network: after the registration lines, the emit file gets one line per near pair, `{"op":"librarian","id":"recheck-<added hash>-<other>","kind":"recheck","proposer":"<owner id of the added concept>","envelope":"<the text the librarian command would read from its --proposal file>"}`, for the landing program to deliver with the librarian command.
- With a DSN, `ledger` runs the librarian command itself, as the gate does.

The JSON output is:

```json
{"commit":"…","contracts":{…},"registry":{"added":[],"defects":[],"near":[],"unable":""}}
```

The final stderr line counts contracts, registrations, contract defects, gone, unowned, ambiguous, and registry additions, near pairs, and defects. `ledger` exits 0 only when both checks are clean; 1 for a contract hash defect, gone, unowned, ambiguous contract, registry defect, or added near pair; 3 when it cannot check the contracts; 75 for a ledger connection failure, and when the registry recheck cannot run at all (no sandbox or toolchain, a missing landing record, an exporter that does not run), whatever else it found, so that the landing program runs the check of that landing again; and 2 for usage.

## Baseline measurements

The following baseline measurements describe the indexed project, not acceptance evidence for this change: 52,529 modules, 545,934 declarations, 9,686,627 uses, 4,389,369 edges, and 789,877 compiled constants. The initial refresh took 15 minutes 43 seconds; an unchanged update took 2.2 seconds; a one-module compiled update took 3.4 seconds. Process-inclusive query times were 0.014 seconds for `where`, 0.2 seconds median and 0.67 seconds worst for `users`, 0.34 seconds for `states`, 0.11 seconds for `same`, and about 5 seconds for `gate`.

Acceptance records live with the acceptance runs, not in this repository.
