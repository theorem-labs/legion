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

Exit codes: 0 done, 1 refused, 2 usage (a missing launch variable included), 3 unable to check (an unreadable graph,
rule, manifest, score, reviews or listener configuration input), 4 protocol error. Item versions are canonical: a
decimal has no leading zero, so `@r01` is no item id.
