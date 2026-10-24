# owner

The `owner` package supplies owner identifiers and path templates, the strict loaders of the copy's
`skeleton/owner.yaml` (read at `main` of the bare repository) and of the operator's deployment file, the record
types, the step runner, the Envoy publisher, the routing table, owners' topic subscriptions, the shared rule and
the inbox that the `owner` command (`cmd/owner`) uses.

## The command

`owner [--json] [--deploy PATH] <command>`. The deployment file defaults to `~/.config/owner/deploy.yaml`. The
command acts for `LEGION_OWNER` in ledger repository `LEGION_LEDGER_REPOSITORY`, with the ledger DSN from the file
`LEGION_LEDGER_DSN_FILE` (the three launch variables of C8; a command that needs one it lacks exits 2), and the
Envoy listener of `ResolveListener`. `RunnerFromEnv` is the one way the command builds its step runner, and the
command builds it before it creates the owner's worktree or writes anything: a listener configuration that cannot be
read is exit 3, with no worktree, branch, record, item or journal line left behind.

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
- A notice about a work item carries the key `hex32(item)` (the first 32 hex digits of SHA-256) and names the item.
  A spec revision that offers no edge queues no item for the users of the spec, so its notice names none: its message
  names the revision, and its key is `hex32(record@commit)`, one for each revision. A re-run repeats the key, and the
  listener's dedupe key `publish.<key>` has Envoy's minted shape (`MINTED_DEDUPE_KEY_PATTERN` in
  `packages/contracts`), so each subscribed session takes the repeated notice once.
- Each commit holds the worktree's commit lock, `flock(2)` on `owner.flock` in the worktree's git directory, from
  its read of `HEAD` to its commit. Its git commands inherit the lock, so it holds until the last of them exits, also
  when the owner command is killed first. A command that holds the lock finds no live git of another owner command
  in the worktree, so it removes the `index.lock`, `HEAD.lock` and branch ref lock that a killed command left: a
  killed step does not wedge its owner, and two commands of one owner wait for each other instead of failing on
  `index.lock`. A person's git command does not take the lock, so none runs in `./wt` while an owner command does.
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
| `inbox` | Open items in `RULE.yaml` order, each with its record, the branch that holds it and the commit to read. The items come from the ledger's `Store.OpenItems` in enqueue order, and items the rule's keys tie keep that order; `enqueued_at` is the position in it |
| `claim <item>` | A supervised lease on an existing item, for a person or an owner outside a step |
| `need <target> <file> [--outside-part \| --answers <reply item>...]` | Writes `needs/E<hex16(target)>.md` (the draft sets `author_model`, `cites` and `resolves`), queues `need:<edge>@r<n>`, notifies by the routing table; `--outside-part` records the edge as `outside_part` with no item; `--answers` answers the target's replies about this need: the revision lists them in `resolves`, carries their trailers, and completes them |
| `offer <file> [--answers <item>...]` | Writes `spec.md` revision n (the draft sets `author_model`, `complete` and `resolves`) and answers each item: a need to the owner, a reply about an edge whose target is the owner (both queue `offer:<edge>@r<n>`, n the spec revision, for the edge's source), or a reply that names the owner its premise owner; a reply is listed in `resolves`. It also answers an open `start:` item, and notifies. When every `--answers` item is done, it repeats the revision that answered them, or refuses a changed draft (exit 1), which would be a new revision that answers none of them |
| `accept <item>` | Accepts `offer:<edge>@r<n>` while the target's spec is at r<n> and answers the current need; records the accept and the edge as `accepted`, answers the offer, notifies |
| `reply <item> <reason> <file> [--premise-owner <id>]` | Records the reply, routes it by `PROTOCOL.yaml` `reasons`; a reason that goes to the premise owner queues the reply item for that owner and sends its notice to the premise owner's needs topic (`node.<id>.needs`, or `fact.<name>.needs`), with `to` the premise owner; `program-defect` and `false-shared-assumption` also write `escalations/<hex16>.yaml` and queue `escalation:` for `escalations` |
| `done <need or offer item> --superseded r<n>` | Completes the item as `superseded:r<n>` with no record, when r<n> is later than the item's and exists: the source's need record, or the target's spec, is at r<n> or newer |
| `returned <item>` | Prints the C13 `reason`, `detail`, `log`, `batch` and `key` of the return event. The producer requires `reason` and `key` (exit 4 without either) and writes an empty `batch`, `detail` or `log` when it was not given one: such a field prints as `(absent)`, and the JSON leaves it out |
| `message-test [--prefix P] [--nodes A,B] [--kills N] [--allow-missing-kinds K,...] --out DIR` | Runs the message test (below) and writes `DIR/report.json` and `DIR/report.md`; exit 0 only when every assertion held |
| `message-test topics` | A test owner's topics: `owner topics` plus the fact and concept topics the driver gives it in the test deployment's `<state_dir>/message-test/topics.json` (plan Task 1.8 step 4: the driver subscribes a test owner to topics no node owner holds); no other command reads that file |
| `message-test subscribed` | Records the topics the test owner subscribed to, at `message-test/subscribed.yaml` |
| `message-test saw <delivery id> <topic>` | Records one delivery once, at `message-test/saw/<hex16(id)>.yaml`, with `Owner-Delivery` and `Owner-Topic` trailers |
| `message-test answer <item>` | Answers a `start:`, `need:` or `offer:` item with an offer record of fixed text as one step (record, the need's follow-up `offer:`, completion). The record has a revision, as a spec does: 1, and one more each time a newer version rewrites it; a need's follow-up is `offer:<edge>@r<n>`, n that revision, as production's `offer` numbers it. An item older than the version already answered completes as `superseded:<newer>` with no record |

Exit codes: 0 done, 1 refused, 2 usage (a missing launch variable included), 3 unable to check (an unreadable graph,
rule, manifest, score, reviews or listener configuration input), 4 protocol error. Item versions are canonical: a
decimal has no leading zero, so `@r01` is no item id.

## The message test

`owner message-test` shows, before production owners start, that a notice on each topic kind reaches owner sessions
that the supervisor starts from their ledger queues, and that duplicate notices and items, items out of order, killed
owner sessions and a supervisor restart leave each step done once (plan Task 1.8). Package `messagetest` drives it:

1. **Setup.** The run's name is `message-test-<UTC stamp>-<8 random hex digits>`, so that two runs started in the
   same second share nothing: it names the ledger repository, the daemon's project and tmux socket, and the private
   bearer directory. The run's topic prefix is `--prefix` (without it, the deployment graph's own prefix), then the
   run's name and a dot, so that runs on one hub never see each other's notices: every topic the owners subscribe to
   and the driver publishes on is under it. A ledger repository of the run's name in the ledger of the operator
   deployment's `ledger_dsn_file` (the driver is the operator's program: it reads no launch variable, and the
   supervisor gives the same file to the test owners as `LEGION_LEDGER_DSN_FILE`), a bare repository with the test's
   `skeleton/` files and the run's prefix as `topic_prefix`, a graph directory with the two nodes' `owners.json`, their
   `topics.json` moved under the run's prefix, the deployment's `graph.json`, and a `facts.json` in which the source
   node states `stack-alignment`; a test deployment `DIR/deploy.yaml`; and the daemon's minimal standing-owners file
   `DIR/daemon/legion.yaml` (C33: `project`, `state_dir`, `runtime: tmux`, its own `port`, `operator_token_file`, the
   Envoy keys, and `standing_owners` with `max_running: 2` and an `omp` template per node kind, prompt
   `DIR/daemon/owner.md`). The daemon's own database comes from `LEGION_POSTGRES_DSN`. It runs
   `legion start --config DIR/daemon/legion.yaml` from `PATH`, with the
   `owner` executable's directory first. The daemon never looks omp up on `PATH`, so the driver gives it
   `LEGION_OMP_PATH`: the operator's `LEGION_OMP_PATH`, else the `omp` on the supervisor's `PATH`; the report names it.
   The daemon refuses to start without an operator bearer, so the run writes a new random one for its operator routes,
   which it never calls. The bearers reach the daemon as files: the Envoy bearer is `ENVOY_TOKEN_FILE` when set, else
   a 0600 file, and the operator bearer is a 0600 file, both in `<state_dir>/message-test/<run>/` of the operator's
   deployment, which the teardown removes; the run writes no token under `DIR` (the daemon's boot tokens there go at
   teardown, step 7). The report records the revisions that
   ran: the owner's build and Go modules, `legion version`, `omp --version` of the omp the panes run, `tmux -V`,
   `git --version` and the SHA-256 of each graph input.
2. **Nodes.** `--nodes A,B`, or else the first function whose id holds `@`, `/` and `.` with a data structure it
   accesses, such that the two hold every topic kind. The code names no project, so the plan's
   `function:get_mountpoint@fs/namespace.c,struct:mountpoint` is passed with `--nodes`. A topic kind (`needs`,
   `callers`, `callees`, `users`, `neighbors`, `linked`, `all`) that no topic of the two nodes has fails the run in
   setup, before the daemon starts, unless `--allow-missing-kinds` names it; the report lists each allowed kind the
   nodes lack.
3. **Wake.** `start:<node>@1` for each; both panes must start within 60 s on the tmux socket
   `legion-<project token>`, each process with `LEGION_OWNER`, `LEGION_LEDGER_REPOSITORY` and
   `LEGION_LEDGER_DSN_FILE` (C8; a `_KEY` name is credential-shaped and the daemon strips it from panes) and working
   in `<state_dir>/owners/<claim token ending in the C9 encoding of the id>`. The driver finds each owner's omp
   session transcript among the files its pane's processes hold open, and publishes only once the listener's
   `GET /v1/sessions` lists that session with every topic the owner must subscribe to: a session registers its topics
   after it opened its subscriptions to them.
4. **Arrival.** One notice on each topic kind the two hold, then on `fact.stack-alignment.users`,
   `fact.stack-alignment.needs` and `concepts` (the source node also subscribes to the last two). Each must reach
   every receiver's session, by its Envoy `event_id`.
5. **Duplicates, out of order, kills, restart** as the plan says. The duplicate need's notice is published twice
   under two idempotency keys, as Envoy may deliver one message twice: both reach the session and the owner answers
   once. The need itself is enqueued twice; when the owner answers it between the two calls, the ledger refuses the
   second as an existing item, which is a duplicate too. The driver expects each answered need's offer by
   production's numbering: the n-th need the target answers queues `offer:<edge>@r<n>`.
   Every notice to a running, subscribed owner is a required delivery: arrival, duplicates, out of order, the
   restart's need when the target's pane runs, and a notice after the daemon restart to the pane it left running.
   A killed owner's need notice is optional, because the kill can end the session before the notice reaches it.
   `--kills` (default 5; at least 3, one at each moment, and fewer is a usage error, exit 2) panes are killed at a
   moment drawn from the owner's progress, and the daemon is restarted
   once while an owner runs. Each step ends when the ledger shows its items' outcomes and the sessions' transcripts
   show its notices.
6. **Check.** `messagetest.Check` reads the ledger's events, the owners' branches and the deliveries the owners'
   session transcripts record, and fails on a missed, duplicate or unexpected delivery (a notice of another run
   included), an item completed twice or never, recorded twice or not at all, an out-of-order item answered as
   current, a follow-up enqueued twice, never or for another owner, and a record or completion of an item the run did
   not expect.
7. **Teardown.** The run stops its daemon by its own PID, its panes by its own tmux socket, and then every owner
   process it saw in its panes that still runs, by its own PID (a PID counts only while its process names the run's
   ledger repository). It then removes its bearer directory and the daemon's boot-token files, which the daemon writes
   under its state directory in `DIR` (`DIR/daemon/state/secrets/`); the report lists those PIDs and paths. On SIGINT
   or SIGTERM the run ends its steps, collects and tears down on a context of its own, and writes a partial report
   marked `INTERRUPTED`; `owner message-test` then exits 1.

A delivery's evidence is the owner session's own record of it: pi-envoy hands each Envoy delivery to the session as
an Oh My Pi custom message of type `envoy-message`, which Oh My Pi appends to the session's JSONL transcript under
`.omp/agent/sessions/`. The message's `details` are pi-envoy's record of the delivery: the subject it arrived on and
its envelope's `topic`, `event_id` and `dedupe_key`. A delivery's topic is that record, never the payload's `topic`
the driver wrote: an envelope topic or subject that is not the published topic fails the run as `unexpected`, and a
driver's notice with no such record stops the collect. pi-envoy's `envoy_inbox` list lives in its process's memory, and the listener does not push to
a session that holds its own NATS connection, so neither keeps a record. Every transcript of the owner's sessions in
that directory counts (a relaunch's and a fork's; a fork's copy of its parent's entries counts once); a subagent's
does not. The owners' own `message-test saw` and `subscribed` records are shown in the report beside the evidence,
and decide nothing: an owner model can handle a burst of deliveries and end its turn without recording one.

`messagetest/owner.md` is the test owners' prompt. Owners publish under the copy's `topic_prefix`, which the
deployment graph's `topics.json` repeats; both accept any prefix under `notifications.` that ends in a dot. A test run
passes a `--prefix` of its own, such as `notifications.example-message-test.`, so that its traffic reaches no
production owner; the run's own name under it keeps one run's traffic from another's.

`internal/ownerharness` is test support that only `_test.go` files import (its guard test checks it), and no command
builds it. The `cmd/owner` test binary runs it when linked as `legion` (a stand-in supervisor in the shape of the
agreed contracts C8, C9 and C33, which refuses to start, as the daemon does, without a readable, non-empty
`operator_token_file` or without the omp that `LEGION_OMP_PATH` names, and never looks omp up on `PATH`) or `omp` (a
scripted owner that follows `owner.md` and keeps an omp-shaped session transcript under `HOME`;
`OWNER_HARNESS_STEP_DELAY` paces it, and `OWNER_HARNESS_SKIP_SAW=<step>` makes it take that step's notices without
recording them), with an HTTP stand-in for the listener's publish and sessions routes. Each stand-in answers a version
request as a test-only stand-in, so the report of a run against it says
so. A run against the harness checks the driver and the owner command; it is neither production nor acceptance
evidence for C8, C9 or C33. Against the real daemon the same run fails at `start the daemon` until the daemon
implements those contracts.
