# Acceptance drivers

These drivers keep the declaration-index acceptance checks reproducible. Run every sandbox probe in a disposable container, never on a shared host or with `sudo`. Acceptance records live with the acceptance runs, not in this repository.

## Build the image

From the repository root, build the image with the root build context. `LEAN_IMAGE` and `LEAN_TOOLCHAIN` have no default; the build stops with an error naming the one that is unset:

```sh
docker buildx build --load --tag declindex-e2e:local \
  --build-arg LEAN_IMAGE=<worker-image>@sha256:<digest> \
  --build-arg LEAN_TOOLCHAIN=<toolchain-name> \
  --file packages/orchestration/declindex/e2e/Dockerfile .
```

The image never builds Lean. It copies the prebuilt toolchain `/opt/lean/<LEAN_FORK_COMMIT>` from `LEAN_IMAGE`, a worker image pinned by digest, and the build fails unless `lean --githash` equals `LEAN_FORK_COMMIT`. `LEAN_TOOLCHAIN` is the toolchain name in the served tree's `lean-toolchain` file, so that elan in the image selects the copied toolchain for that tree. Record the `LEAN_IMAGE` digest with each run. `build-lean.sh` builds Lean from source for CI runners only.

The base is the digest-pinned `golang:1.27.1-trixie` image, because the prebuilt toolchain links the system `libgmp` and `libstdc++` of Debian trixie; the build checks that `libgmp10` is 6.3.0 or newer, which Lean requires. The image installs elan v4.2.4 after checking its SHA-256, links the toolchain as `LEAN_TOOLCHAIN` and makes it the default, and installs `declindex`, `ledger`, `python3` and `strace`.

The checksum's negative control is the same build with one digit of `ELAN_SHA256` changed; it must fail at `sha256sum --check`, before `elan-init` runs:

```sh
docker buildx build --tag declindex-e2e:bad-elan \
  --build-arg LEAN_IMAGE=<worker-image>@sha256:<digest> \
  --build-arg LEAN_TOOLCHAIN=<toolchain-name> \
  --build-arg ELAN_SHA256=52b94d4244e8353142c456ec0e4ca6528fd898a6c604d4059f494e706e431f63 \
  --file packages/orchestration/declindex/e2e/Dockerfile .
```

## Sandbox acceptance

Create one empty run directory, here `$RUN`, before starting the driver. The driver records the image ID, `docker version`, the kernel release, `lean --version`, each case's output, the Landlock `strace`, and `sandbox.tsv` there.

```sh
mkdir -p "$RUN"
packages/orchestration/declindex/e2e/sandbox-e2e.sh \
  --image declindex-e2e:local \
  --out "$RUN"
```

The driver starts one-shot containers only, each as the invoking user. The first copies `declindex/testdata/lean` into the run's `work/tree`, commits it with an empty registry, builds it with Lake, and refreshes a compiled index; each probe then gets its own copy of that index, without an exporter. The probes mount `work/tree` read-only. The driver prints one TSV row for each case and exits 1 unless all five pass:

| Case | Expected result |
| --- | --- |
| `eval` | Exit 3; `/out/pwned-eval` is absent. |
| `char` | Exit 3; `/out/pwned-eval` is absent. |
| `elab` | Exit 3; `/out/pwned-elab` is absent. |
| `benign` | Exit 0 with an `added` verdict against the read-only tree, which `git status --porcelain` still shows clean. |
| `deny-landlock` | Exit 3 with `Landlock` on stderr, exactly one `execve` or `execveat` in the trace, and no `exporter` directory in the gate's state. |

The first four cases use Docker's default seccomp profile. The last passes `deny-landlock.json`, which allows every syscall except the three Landlock syscalls; each of those fails with `ENOSYS` (errno 38).

## Recall

`recall` measures unique files, not declarations. `truth.txt` is a newline-delimited list of paths relative to the indexed tree; blank lines and `#` comments are ignored. The `states` input is `declindex states --json` output. `extras.tsv` has one deterministic first declaration and location per file that `states` finds outside the ground truth.

```sh
go run ./packages/orchestration/declindex/e2e/recall \
  --states states.json \
  --concept Example.Concept \
  --truth truth.txt \
  --extras extras.tsv
```

The output gives `truth_files`, `state_files`, `matched_files`, `missing_files`, `extra_files`, and `recall`. The driver rejects an empty ground-truth set because recall would be undefined.

## Parity with `uses`

Capture both tools for the same full declaration names and index revision, then compare the resulting JSON:

```sh
go run ./packages/orchestration/declindex/e2e/parity \
  --declindex declindex-users.json \
  --uses uses-users.json \
  --names names.txt
```

`names.txt` is a newline-delimited list of full declaration names. The driver checks direct-use totals, every per-directory and file-kind count, and every reach count. It prints one TSV discrepancy for each differing field and exits 1 when any requested declaration is absent or differs.

## Librarian recording

Configure a gate's `librarian` command with `record-librarian`; the gate calls it with its own arguments, as below. The record path is supplied by `LIBRARIAN_RECORD`.

```sh
LIBRARIAN_RECORD=/path/to/librarian-record \
packages/orchestration/declindex/e2e/record-librarian \
  --id near-0123456789abcdef \
  --proposer function:alloc_mnt_ns \
  --proposal /path/to/request
```

It appends `<UTC>\t<id>\t<proposer>\t<SHA-256>` to the record, copies the proposal to `$LIBRARIAN_RECORD.d/<id>`, and exits 0.

To record in front of a real librarian, configure `librarian: [record-librarian, --, COMMAND, ARGS...]`. The gate appends `--id … --proposer … --proposal …` to that list, so `record-librarian` takes everything between the leading `--` and those six arguments as the command. It records the call, then runs `COMMAND ARGS... --id … --proposer … --proposal …`, and exits with its status.

## Ledger join

Run the ledger join where `declindex`, `ledger`, Python 3, the configuration's indexed tree, and the ledger DSN are reachable. `JOIN_CANDIDATE_COMMIT` and `JOIN_CHANGED_HASH` must name the already prepared, real candidate; the driver does not invent a source mutation.

```sh
JOIN_CANDIDATE_COMMIT=<40-lowercase-hex-commit> \
JOIN_CHANGED_HASH=<16-lowercase-hex-statement-hash> \
packages/orchestration/declindex/e2e/ledger-join.sh \
  /path/to/declindex.yaml \
  <ledger-repository> \
  Example.Concept \
  function:alloc_mnt_ns \
  /path/to/ledger-dsn
```

The driver ensures the repository with a three-round limit, then runs steps 1 through 6 in order: register, open and verify `join-a`, open `join-b`, land `join-a` at version 2, open and verify `join-c` at version 2, then require the stale-read refusal for `join-b`. It exits at the first deviation. `DECLINDEX_BIN` and `LEDGER_BIN` may override the commands on `PATH`.

## Driver tests

Run the deterministic Go-driver tests from `packages/orchestration`:

```sh
go test ./declindex/e2e/...
```
