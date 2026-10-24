# owner

The `owner` package supplies owner identifiers and path templates, the strict loaders of the copy's
`skeleton/owner.yaml` (read at `main` of the bare repository) and of the operator's deployment file, the record
types, the step runner, the Envoy publisher, the routing table, owners' topic subscriptions, the shared rule and
the inbox that the `owner` command (`cmd/owner`) uses.

## The command

`owner [--json] [--deploy PATH] <command>`. The deployment file defaults to `~/.config/owner/deploy.yaml`. The
command acts for `LEGION_OWNER` in ledger repository `LEGION_LEDGER_REPOSITORY`, with the ledger DSN from the file
`LEGION_LEDGER_DSN_FILE` (the three launch variables of C8; a command that needs one it lacks exits 2), and the
Envoy listener of `ResolveListener`. `RunnerFromEnv` is the one way the command builds its step runner, and it
resolves the listener before a step writes anything: a listener configuration that cannot be read is exit 3.

Inputs:

- **The graph** comes from the deployment's `graph_dir`. `owner serve`'s landing step copies `owners.json`,
  `topics.json`, `graph.json` and `measures.json` there after each landing (plan Task 2.2, step 2), and Task 3.2
  writes `facts.json` there. A missing `owners.json`, `topics.json` or `graph.json` is exit 3; a missing
  `measures.json` or `facts.json` means no interface mappings or fact users yet.
- **The score** is the newest `<score_part>/<commit>.json` on the bare repository's `score` branch, which the
  landing step fetches there (Task 2.2, step 1). The rule reads its `skeleton.node_instructions`, keyed by node
  id. Before the first score the branch does not exist, and every stopped-instruction count is 0 (`RULE.yaml`).

Steps:

- A record commit carries one `Owner-Item: <item>` trailer for each item it answers: a spec revision that answers
  two needs and the open `start:` item carries three. An item never completes with a commit that lacks its
  trailer (`ErrUnattributed`, exit 4).
- A step that answers no item (`need`, a spec revision with no open item) commits its record with no trailer,
  queues its follow-ups and sends its notices.
- A notice that the listener still refuses after retries at 1 s, 4 s and 16 s does not fail the step: the command
  prints `notice not delivered: <topic>` on stderr and appends the notice to `<state_dir>/undelivered.jsonl`.
- A claim keeps its `[C<n>]` id while its owner revises it. An id that a revision removes is retired: the command
  records it in the front matter's `retired`, and refuses a later revision that uses it again.
- A need's front matter is the plan's P3 record (`source` and `target` are the edge's endpoints) plus `origin:
  neighbor`, `resolves` and `retired`. A review finding carries `origin: review` and its `node`. Measure 6 reads the
  origin, and the record kind fixes it: needs and replies come from a neighbor, findings from review.

| Command | Does |
| --- | --- |
| `whoami` | The owner's id, role, key, kind, Lean directory, C file, active set and active epoch |
| `topics` | `topics.json`'s list, plus `concepts` for the librarian, `fact.<name>.needs` for a fact owner, `fact.<name>.users` for each fact the owner states, and `all` for every owner |
| `worktree` | Creates or reuses `./wt` on `owner/<Key>` of the bare repository |
| `inbox` | Open items in `RULE.yaml` order, each with its record, the branch that holds it and the commit to read |
| `claim <item>` | A supervised lease on an existing item, for a person or an owner outside a step |
| `need <target> <file> [--outside-part \| --answers <reply item>...]` | Writes `needs/E<hex16(target)>.md` (the draft sets `author_model`, `cites` and `resolves`), queues `need:<edge>@r<n>`, notifies by the routing table; `--outside-part` records the edge as `outside_part` with no item; `--answers` answers the target's replies about this need: the revision lists them in `resolves`, carries their trailers, and completes them |
| `offer <file> [--answers <item>...]` | Writes `spec.md` revision n (the draft sets `author_model`, `complete` and `resolves`) and answers each item: a need to the owner, a reply about an edge whose target is the owner (both queue `offer:<edge>@r<n>`, n the spec revision, for the edge's source), or a reply that names the owner its premise owner; a reply is listed in `resolves`. It also answers an open `start:` item, and notifies |
| `accept <item>` | Accepts `offer:<edge>@r<n>` while the target's spec is at r<n> and answers the current need; records the accept and the edge as `accepted`, answers the offer, notifies |
| `reply <item> <reason> <file> [--premise-owner <id>]` | Records the reply, routes it by `PROTOCOL.yaml` `reasons`; `program-defect` and `false-shared-assumption` also write `escalations/<hex16>.yaml` and queue `escalation:` for `escalations` |
| `done <need or offer item> --superseded r<n>` | Completes the item as `superseded:r<n>` with no record, when r<n> is later than the item's and exists: the source's need record, or the target's spec, is at r<n> or newer |
| `returned <item>` | Prints the C13 `reason`, `detail`, `log`, `batch` and `key` of the return event |
| `message-test [--prefix P] [--nodes A,B] [--kills N] --out DIR` | Runs the message test (below) and writes `DIR/report.json` and `DIR/report.md`; exit 0 only when every assertion held |
| `message-test topics` | A test owner's topics: `owner topics` plus the fact and concept topics the driver gives it in the test deployment's `<state_dir>/message-test/topics.json` (plan Task 1.8 step 4: the driver subscribes a test owner to topics no node owner holds); no other command reads that file |
| `message-test subscribed` | Records the topics the test owner subscribed to, at `message-test/subscribed.yaml` |
| `message-test saw <delivery id> <topic>` | Records one delivery once, at `message-test/saw/<hex16(id)>.yaml`, with `Owner-Delivery` and `Owner-Topic` trailers |
| `message-test answer <item>` | Answers a `start:`, `need:` or `offer:` item with an offer record of fixed text as one step (record, the need's follow-up `offer:`, completion); an item older than the version already answered completes as `superseded:<newer>` with no record |

Exit codes: 0 done, 1 refused, 2 usage (a missing launch variable included), 3 unable to check (an unreadable graph,
rule, manifest, score, reviews or listener configuration input), 4 protocol error. Item versions are canonical: a
decimal has no leading zero, so `@r01` is no item id.

## The message test

`owner message-test` shows, before production owners start, that a notice on each topic kind reaches owner sessions
that the supervisor starts from their ledger queues, and that duplicate notices and items, items out of order, killed
owner sessions and a supervisor restart leave each step done once (plan Task 1.8). Package `messagetest` drives it:

1. **Setup.** A ledger repository `message-test-<UTC stamp>` (the ledger of `LEGION_LEDGER_DSN_FILE`, else the
   operator deployment's `ledger_dsn_file`), a bare repository with the test's `skeleton/` files, a graph directory with the
   two nodes' `owners.json`, their `topics.json` moved under `--prefix`, the deployment's `graph.json`, and a
   `facts.json` in which the source node states `stack-alignment`; a test deployment `DIR/deploy.yaml`; and the
   daemon's minimal standing-owners file `DIR/daemon/legion.yaml` (C33: `project`, `state_dir`, `runtime: tmux`,
   its own `port`, the Envoy keys, and `standing_owners` with `max_running: 2` and an `omp` template per node kind,
   prompt `DIR/daemon/owner.md`). The daemon's own database comes from `LEGION_POSTGRES_DSN`. It runs
   `legion start --config DIR/daemon/legion.yaml` from `PATH`, with the `owner` executable's directory first. The
   Envoy bearer reaches the daemon as `envoy_token_file`: `ENVOY_TOKEN_FILE` when set, else a 0600 file in
   `<state_dir>/message-test/<run>/` of the operator's deployment, which the teardown removes; no token is written
   under `DIR`. The report records the revisions that ran: the owner's build and Go modules, `legion version`,
   `omp --version`, `tmux -V`, `git --version` and the SHA-256 of each graph input.
2. **Nodes.** `--nodes A,B`, or else the first function whose id holds `@`, `/` and `.` with a data structure it
   accesses, such that the two hold every topic kind. The code names no project, so the plan's
   `function:get_mountpoint@fs/namespace.c,struct:mountpoint` is passed with `--nodes`.
3. **Wake.** `start:<node>@1` for each; both panes must start within 60 s on the tmux socket
   `legion-<project token>`, each process with `LEGION_OWNER`, `LEGION_LEDGER_REPOSITORY` and
   `LEGION_LEDGER_DSN_FILE` (C8; a `_KEY` name is credential-shaped and the daemon strips it from panes) and working
   in `<state_dir>/owners/<claim token ending in the C9 encoding of the id>`; each must record its subscriptions.
4. **Arrival.** One notice on each topic kind the two hold, then on `fact.stack-alignment.users`,
   `fact.stack-alignment.needs` and `concepts` (the source node also subscribes to the last two). Every receiver must
   record each notice, by its Envoy `event_id`.
5. **Duplicates, out of order, kills, restart** as the plan says. The duplicate need's notice is published twice
   under two idempotency keys, as Envoy may deliver one message twice: the owner records both and answers once.
   Every notice to a running, subscribed owner is a required delivery: arrival, duplicates, out of order, the
   restart's need when the target's pane runs, and a notice after the daemon restart to the pane it left running.
   A killed owner's need notice is optional, because the kill can end the session before it records it.
   `--kills` (default 5) panes are killed at a moment drawn from the owner's progress, and the daemon is restarted
   once while an owner runs.
6. **Check.** `messagetest.Check` reads the ledger's events and the owners' branches and fails on a missed,
   duplicate or unexpected delivery, an item completed twice or never, recorded twice or not at all, an out-of-order
   item answered as current, a follow-up enqueued twice, never or for another owner, and a record or completion of an
   item the run did not expect.

`messagetest/owner.md` is the test owners' prompt. Production owners publish under `notifications.lxspec.`; a test
run uses `notifications.lxspec-<name>.`, which `topic_prefix` and `topics.json` accept so that its traffic reaches no
production owner.

`internal/ownerharness` is test support that only `_test.go` files import (its guard test checks it), and no command
builds it. The `cmd/owner` test binary runs it when linked as `legion` (a stand-in supervisor in the shape of the
agreed contracts C8, C9 and C33) or `omp` (a scripted owner that follows `owner.md`; `OWNER_HARNESS_STEP_DELAY`
paces it), with an HTTP stand-in for the listener. Each stand-in answers a version request as a test-only stand-in,
so the report of a run against it says so. A run against the harness checks the driver and the owner command; it is
neither production nor acceptance evidence for C8, C9 or C33. Against the real daemon the same run fails at
`start the daemon` until INFRA-137 R1 lands those contracts.
