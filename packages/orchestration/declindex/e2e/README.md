# Acceptance drivers

These drivers keep the declaration-index acceptance checks reproducible. Acceptance records live with the acceptance runs, not in this repository.

## Build the image

From the repository root, build the image with the root build context. `LEAN_IMAGE` and `LEAN_TOOLCHAIN` have no default; the build stops with an error naming the one that is unset:

```sh
docker buildx build --load --tag declindex-e2e:local \
  --build-arg LEAN_IMAGE=<worker-image>@sha256:<digest> \
  --build-arg LEAN_TOOLCHAIN=<toolchain-name> \
  --file packages/orchestration/declindex/e2e/Dockerfile .
```

The image never builds Lean. It copies the prebuilt toolchain `/opt/lean/<LEAN_FORK_COMMIT>` from `LEAN_IMAGE`, a worker image pinned by digest, and the build fails unless `lean --githash` equals `LEAN_FORK_COMMIT`. `LEAN_TOOLCHAIN` is the toolchain name in the served tree's `lean-toolchain` file, so that elan in the image selects the copied toolchain for that tree. Record the `LEAN_IMAGE` digest with each run. `build-lean.sh` builds Lean from source for CI runners only.

The base is the digest-pinned `golang:1.27.1-trixie` image, because the prebuilt toolchain links the system `libgmp` and `libstdc++` of Debian trixie; the build checks that `libgmp10` is 6.3.0 or newer, which Lean requires. The image installs elan v4.2.4 after checking its SHA-256, links the toolchain as `LEAN_TOOLCHAIN` and makes it the default, and installs `declindex`, `ledger` and `python3`.

The checksum's negative control is the same build with one digit of `ELAN_SHA256` changed; it must fail at `sha256sum --check`, before `elan-init` runs:

```sh
docker buildx build --tag declindex-e2e:bad-elan \
  --build-arg LEAN_IMAGE=<worker-image>@sha256:<digest> \
  --build-arg LEAN_TOOLCHAIN=<toolchain-name> \
  --build-arg ELAN_SHA256=52b94d4244e8353142c456ec0e4ca6528fd898a6c604d4059f494e706e431f63 \
  --file packages/orchestration/declindex/e2e/Dockerfile .
```

## Recall

`recall` measures unique files, not declarations. `truth.txt` is a newline-delimited list of paths relative to the indexed tree; blank lines and `#` comments are ignored. The `states` input is the output of `declindex --config <cfg> --json states <concept>`. `extras.tsv` has one deterministic first declaration and location per file that `states` finds outside the ground truth.

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

Run the ledger join where `declindex`, `ledger`, Python 3, the configuration's indexed tree, the index's Postgres server and the ledger DSN are reachable. The configuration names the index's schema with `index:`; the last two arguments are the two connection strings, the ledger's and the index's. `JOIN_CANDIDATE_COMMIT` and `JOIN_CHANGED_HASH` must name the already prepared, real candidate; the driver does not invent a source mutation.

```sh
JOIN_CANDIDATE_COMMIT=<40-lowercase-hex-commit> \
JOIN_CHANGED_HASH=<16-lowercase-hex-statement-hash> \
packages/orchestration/declindex/e2e/ledger-join.sh \
  /path/to/declindex.yaml \
  <ledger-repository> \
  Example.Concept \
  function:alloc_mnt_ns \
  /path/to/ledger-dsn \
  /path/to/index-dsn
```

The driver prints the index's schema, ensures the repository with a three-round limit, then runs steps 1 through 6 in order: register, open and verify `join-a`, open `join-b`, land `join-a` at version 2, open and verify `join-c` at version 2, then require the stale-read refusal for `join-b`. It exits at the first deviation. `DECLINDEX_BIN` and `LEDGER_BIN` may override the commands on `PATH`.

## Query timings

`timings.sh` holds the commands to the acceptance bars of one index. It runs `where`, `users`, `states`, `same`, `land-hash`, `gate` and `ledger` for each declaration of the sample, each under `/usr/bin/time`, and prints one `run` line for each run (command, declaration, seconds, exit status) and a `summary` line for each command (runs, how many exited 0, median, 95th percentile, worst). It exits 1 when the median of `users` reaches 1 s or its 95th percentile reaches 3 s, or when the median of `where`, `states` or `same` reaches 1 s. `land-hash`, `gate` and `ledger` are reported with no bar.

Write the sample first. It is 50 declarations drawn with a fixed seed and the 50 declarations that most statements use, so that two runs of one index measure the same work:

```sh
go run ./packages/orchestration/declindex/e2e/sample \
  --config /path/to/declindex.yaml \
  --index-dsn-file /path/to/index-dsn \
  --out sample.txt
```

```sh
packages/orchestration/declindex/e2e/timings.sh \
  --index-dsn-file /path/to/index-dsn \
  /path/to/declindex.yaml sample.txt
```

`--limit N` measures the sample's first N declarations, for a quick run. `states` answers for a registered concept, which a sample of declarations need not hold: `--concepts FILE` times `states` over those names instead, one for each line, and without it the driver prints a `note` line whenever no run of a command under a bar answered, so that nobody reads a bar that timed only refusals. Without `--gate-proposal FILE` the driver writes a fresh proposal for each declaration, so that the gate does its whole work; with it, every gate run reads that one proposal. `--proposer OWNER` names the gate's proposer. `DECLINDEX_DSN` in the environment replaces `--index-dsn-file`, and `DECLINDEX_BIN` the command on `PATH`.

The refresh bar is a mode of its own: it times a refresh to an already landed commit that changes one module and no definition, and holds the index's own work, which the refresh reports as `elapsed_ns` and which excludes the fetch, the checkout and the build, under 30 s.

```sh
TIMINGS_LAND_COMMIT=<40-lowercase-hex-commit> \
packages/orchestration/declindex/e2e/timings.sh \
  --index-dsn-file /path/to/index-dsn \
  --refresh-landing /path/to/declindex.yaml
```

## Driver tests

Run the deterministic Go-driver tests from `packages/orchestration`:

```sh
go test ./declindex/e2e/...
```
