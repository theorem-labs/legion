# owner

The `owner` package is the owner protocol's library: work-item identifiers and notice keys, the strict loaders of the
copy's `skeleton/owner.yaml` (read at `main` of a bare repository) and of the host's deployment file, the path
templates the configuration names, git plumbing over a bare repository, the Envoy publisher and the listener's URL
and token resolution, the routing table and its notices, and notice delivery with its journal. The `owner` command
(`cmd/owner`) uses it. Its one command so far is `librarian-intake`.

## The librarian's intake

```
owner [--json] librarian-intake --id <id> --proposer <owner id> --proposal <file> --ledger-dsn-file <path>
  --repository <name> [--git-dir <repo.git>] [--envoy-config <path>] [--topic-prefix <prefix>] [--deploy <path>]
```

The concept gate delivers each near match to its configured `librarian` command, here `owner librarian-intake`, with
`--id`, `--proposer` and `--proposal <file>` appended, no stdin and 120 s (`conceptgate/librarian.go`): `declindex
gate` does so itself, and so does `declindex ledger` with a DSN; with `--emit`, `declindex ledger` writes each recheck's
id, proposer and envelope to its emit file, and the landing program delivers it the same way. The gate removes the file
when the command exits, so the command reads it whole at once, and it never reads stdin. It acts for the gate: it needs
no `LEGION_OWNER`, and it queues for `librarian`.

- **The ids.** `--id` is `near-<hex16 of the proposal file's bytes>` (the gate) or `recheck-<key>-<key>` (the
  recheck), each key a compiled hash (32 lowercase hex digits) or `name.` and the lowercase hex of a concept name's
  bytes. It holds no `@`. `--proposer` passes the gate's owner-id rule: a `function:`, `struct:`, `global:`,
  `percpu:` or `subsystem:` node id whose name has no white space, or a role name (`[a-z][a-z0-9-]*`).
- **The envelope is untrusted data.** The file is the gate's text: header lines with exactly one `id:` line equal to
  `--id` and one `proposer:` line equal to `--proposer`; one `-----BEGIN UNTRUSTED PROPOSAL <nonce>-----` line and one
  `-----END UNTRUSTED PROPOSAL <nonce>-----` line with the same 32-digit lowercase hex nonce, the end marker the last
  line; and exactly one line between them, the JSON object `{kind, verdict, proposal}` with each key once, `kind` the
  id's kind (`near` or `recheck`) and `verdict` and `proposal` objects. Any line that starts like a marker is one, so a
  second marker is refused; marker text inside the JSON line is data, since the gate's JSON escapes every newline.
  Anything else is exit 2, and nothing is queued or written.
- **The item** is `concept:<proposer>@<id>` for `librarian` in ledger repository `--repository` (its version is after
  the last `@`, so a proposer may hold `@`). The command checks that the ledger repository exists and reads the item
  first. An item in another owner's queue is exit 4. A done item of `librarian` is a success that writes and
  publishes nothing, whatever the envelope says now. A missing item is queued, and the notice of a missing or still
  open item is published under its one key: a delivery that finds the item open may follow one that queued it and then
  lost its notice (a journal that failed, a reply lost at the deadline, a kill), and the listener drops a copy it
  already has. `added` is true when this delivery's enqueue made the item; two deliveries of one id at once can both
  report it, since the ledger's `Enqueue` accepts an item already open for the same owner.
- **A near delivery** records nothing. The proposal is on the proposer's branch at
  `skeleton/nodes/<Key>/concepts/<hex16>.json`, `<hex16>` the id after `near-`, and the gate's verdict beside it as
  `<hex16>.verdict.json`; the proposer's concept command writes both.
- **A recheck delivery** has no proposer copy, so it needs `--git-dir`, the librarian's bare repository (exit 2
  without it, exit 3 when it is no bare repository, exit 2 when a deployment file names another `bare_repo`). Before it
  queues the item, the command writes the record `inbox/<id>.json` on the branch `inbox/librarian` with plumbing only,
  no worktree: a private index from the branch's tip, `commit-tree` on that tip with the repository's identity, then
  `update-ref --no-deref <new> <old>`, again from the new tip when another writer moved it first. A ref lock that
  leaves the tip where it was is waited for up to 5 s, then named in the error; a symbolic ref there is refused. The
  record is `{"untrusted": true, "id", "proposer", "kind", "verdict", "proposal"}`, the verdict and the proposal as the
  gate wrote them. A record of that id is never rewritten: the first envelope stays. The recheck id names no proposer,
  so a record whose `proposer` is not `--proposer` is exit 4. Only this command writes the branch.
- **The notice** goes to `<topic prefix>concepts` (the routing table's concept row) with the payload `type: concept`,
  `from` the proposer, `to: librarian`, the item, and for a recheck its inbox record and commit, under the key
  `NoticeKey(item)`. The command reads no protocol manifest, so its `epoch` is 0, and it has no versions or models to
  name. The topic prefix is `--topic-prefix`, else `topic_prefix` of `skeleton/owner.yaml` at `main` of `--git-dir`,
  whose `repo` must be `--repository` (exit 2 otherwise); with neither, the command exits 2, and a configuration that
  does not load is exit 3, before it queues anything. The listener is `ResolveListener`'s, with `--envoy-config` as
  `ENVOY_CONFIG_FILE`; an `--envoy-config` that cannot be read or decoded is exit 3. A notice that the listener
  refuses is tried again at 1 s, 4 s and 16 s.
- **The journal.** When no listener resolves, or the listener still refuses after the retries, the command prints
  `notice not delivered: <topic> <notice>` on stderr, appends the notice to the host's journal and exits 0: the ledger
  item is the record. The journal is `<state_dir>/undelivered.jsonl` of the deployment file (`--deploy`, else
  `~/.config/owner/deploy.yaml`) when there is one, else `<user cache directory>/owner/undelivered/undelivered.jsonl`
  (Go's `os.UserCacheDir`: `$XDG_CACHE_HOME`, else `~/.cache`). Each line is one notice: its `topic`, `message`,
  `payload` and `key`, and the `error` that kept it from the listener. The command resolves the journal and opens it
  for appending before it writes anything else; a named deployment file that is missing, one that does not load, and
  a journal that cannot be opened are exit 3. A notice that the journal then fails to keep is still printed, and the
  command exits 3.
- **The ledger** is the DSN in `--ledger-dsn-file`, opened with `OpenLedgerFile` (`ledger.Open`, which applies the
  schema versions the database lacks).
- **Time.** Each ledger call (the open, the repository check, the item's read, the enqueue) has 20 s; a notice's four
  attempts take at most 61 s; the whole command stops at 110 s, inside the gate's 120 s, and a delivery that its own
  deadline ended is exit 75.

The librarian reads its queue from the ledger: `ledger items <repository> --owner librarian` lists its open items in
enqueue order, a recheck item's record is `inbox/<id>.json` on `inbox/librarian` of the librarian's bare repository,
and a near item's records are on the proposer's branch.

Exit codes of `librarian-intake`: 0 delivered (queued now, already open, or done); 2 usage, a malformed envelope, or
inputs that disagree (no topic prefix, a configuration of another repository, a `--git-dir` that is not the
deployment's); 3 unable to check (a DSN file that cannot be read, no such ledger repository, a query the server
refuses, a deployment file, configuration, `--envoy-config` or journal that cannot be read, a `--git-dir` that is no
bare repository, a git write that fails); 4 protocol error (the item is another owner's, or a recheck record is
another proposer's); 75 when the ledger cannot be reached (a refused, broken or timed-out connection, a connection
exception, a server that is shutting down, starting up or out of connections) or the command's deadline ended it, so
that the caller retries. The gate fails the delivery on any code but 0.
