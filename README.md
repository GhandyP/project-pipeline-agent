# project-pipeline-agent

A Go web app on Google's Agent Development Kit (ADK for Go, v2) that reviews a
project pipeline, and shows exactly where a model stops and a human starts.

Deterministic Go rules derive what is wrong with a project — severity, owner
state and evidence included. A Gemini agent reads that verdict, explains it,
rank-orders the next actions and proposes at most one dry-run update. **No tool
the agent can call writes anything.** A tracker change happens only after a
configured project owner approves the exact change set, and the tracker re-checks
the project before it writes.

## Why this exists

This repository is a portfolio example of one engineering position: **a model
should be able to explain and propose, but it should not be the thing that
decides what is true or what gets written.**

Everything else follows from that:

- The rules that produce a blocker are pure functions of a snapshot, a policy
  and the current time. They have no I/O, no model and no clock of their own, so
  they are proven in tests instead of trusted.
- The agent's tool surface is `get_project_status` (read), `propose_update`
  (dry run) and `submit_review` (the grounding gate). There is no write tool, so
  a model that decides to change a project has nowhere to send the request.
- Every model intention passes one deterministic gate. A next action must cite a
  blocker the rules actually derived; a change must target an allowlisted field
  with a valid value; an owner must be on the project team. Everything refused is
  reported to the reader with the reason, never silently dropped.
- A tracker update carries the version it was built against and a deterministic
  id derived from the mutation itself. A project that moved is refused instead
  of overwritten, and replaying an approval writes nothing the second time.

## The flow

```text
snapshot ──► rules (pure Go) ──► findings ──► agent explains + prioritizes
   │                                              │
   │                                              ▼
   │                                       dry-run proposal
   │                                              │
   └────────────── tracker ◄── configured owner approval ─┘
                   (version check + idempotency + audit trail)
```

## Architecture

```text
cmd/pipeline              CLI: serve, review, demo, verify, adk
internal/domain           The contract and the pure rules. No I/O.
                          ProjectSnapshot -> Findings -> PipelineReview,
                          change validation, idempotency key, grounding gate.
internal/ports            The boundaries: Tracker, Clock, ReviewStreamer.
internal/tracker          JSON file store: versions, idempotency ledger,
                          atomic writes, append-only audit trail.
internal/service          Use cases: findings, ground, propose, decide.
internal/agent            ADK agent, the three tools, the streaming reviewer,
                          and a deterministic scripted model for offline runs.
internal/web              HTTP API, SSE stream, embedded UI, approval endpoint.
testdata/projects         Six project fixtures: healthy, overdue, blocked,
                          missing-owner, contradictory and empty.
```

The dependency direction only ever points inward: `cmd` depends on the adapters,
the adapters depend on `service`, and `service` depends on `domain` and `ports`.
A fake tracker and a fake streamer run the entire web app in tests with no file,
no credential and no network.

## The safety model in one table

| Step | Reads the tracker | Writes the tracker | Needs a named human |
| --- | --- | --- | --- |
| `get_project_status` (agent tool) | yes | no | no |
| `propose_update` (agent tool) | yes | no | no |
| `submit_review` (agent tool) | yes | no | no |
| `pipeline review` (terminal) | yes | no | no |
| `POST /updates` (dry run) | yes | no | no |
| `POST /updates/{id}/decision` with `approve:false` | yes | no | yes, recorded |
| `POST /updates/{id}/decision` with `approve:true` | yes | **yes** | yes, recorded |
| `pipeline adk` (ADK console/web) | yes | no | no |

`propose_update` stores a proposal file and a `proposed` audit entry so a human
can decide on it later. It never touches project data.

A decision without a name of at least two characters is refused by the web handler;
the tracker independently checks approval authority before it writes. The project's
`owner_id` must identify a team member, and only that owner's full name or id (case
and repeated-space normalized) may approve. Other team members and outsiders receive
`403`. A missing or unrecognized `owner_id` fails closed for approval, while the
snapshot remains readable, reviewable and proposable; an operator must set the owner
explicitly, with no implicit fallback or automatic migration. The submitted name is
accountability rather than authentication, as [docs/missing-work.md](docs/missing-work.md)
says.

## Quick start

The offline path needs no credential and no network:

```bash
go run ./cmd/pipeline demo --project delta-onboarding --approve --as "Ana Ruiz"
```

That seeds six fixture projects into a temporary directory, runs the real ADK
runner with a deterministic scripted model, prints the rules-derived findings and
the proposed dry run, records the approval and shows the audit trail. Without
`--approve` it asks on the terminal, and with no answer it declines and writes
nothing.

The web app:

```bash
go run ./cmd/pipeline serve --scripted        # http://127.0.0.1:8080
```

`serve` seeds `testdata/projects` into `./data` the first time it finds the store
empty, and then never overwrites a project. Drop `--scripted` to use Gemini:

```bash
export GOOGLE_API_KEY="your-key"              # and optionally ADK_MODEL
go run ./cmd/pipeline serve
```

The framework's own console and web UI over the same agent:

```bash
go run ./cmd/pipeline adk console
go run ./cmd/pipeline adk web api webui
```

Configuration check, which contacts nothing:

```bash
go run ./cmd/pipeline verify
```

## What the rules decide

Five rules, each with its own severity table and evidence. Nothing here is
model output.

| Rule | Fires when | Severity |
| --- | --- | --- |
| `overdue` | an open milestone is past its due date | medium, high after 7 days, critical after 30 |
| `blocked_dependency` | an open milestone depends on one that is not done | medium, high when the dependency is late |
| `missing_owner` | an open milestone has no owner | low when planned, medium otherwise |
| `contradictory_status` | `done` at less than 100%, 100% while open, or a blocked reason that does not match the status | medium to high |
| `stalled` | an open milestone has not been updated inside the SLA window | low when planned, medium otherwise |

Every blocker carries a stable id (`kind:milestone`), an owner state and
rule-level evidence. Closed milestones are history: they are never late, blocked
or unowned, and they never accept an update.

## What the model may author, and what it may not

The model writes a summary, ranked next actions and change intentions. It cannot
write severity, an owner state, evidence, a blocker, or a field change that the
gate refuses. Concretely:

- a next action with no blocker id, or with an id the rules did not derive, is
  refused and shown as a refusal;
- an owner that is not on the project team is refused;
- a change to a field outside the allowlist (`owner_id`, `status`, `due_date`,
  `progress_percent`, `blocked_reason`) is refused;
- a change that would create the very contradiction the review reports — `done`
  at 40%, or 100% while in progress — is refused;
- a change set for one milestone is decided as a unit: the gate refuses the whole
  set when the milestone it would leave behind would contradict itself, so a
  status travels with the reason or the progress that belongs to it;
- more than seven ranked actions is refused;
- if the model never submits a review, the rules' findings are shown anyway.

## API

| Method and path | Purpose |
| --- | --- |
| `GET /api/projects` | project list with health and blocker counts |
| `GET /api/projects/{id}` | snapshot and findings |
| `POST /api/projects/{id}/review` | Server-Sent Events stream of one agent review |
| `POST /api/projects/{id}/updates` | validate and store a dry-run update |
| `GET /api/projects/{id}/updates/{updateID}` | load a stored dry run |
| `POST /api/projects/{id}/updates/{updateID}/decision` | record the human decision; the only path that can write |
| `GET /api/projects/{id}/audit` | append-only trail, oldest first |

Project and update ids must match `^[A-Za-z0-9_-]{1,64}$`; a request body is
capped at 64 KiB; a conflict (a project that moved, or an update already decided)
answers `409`; an anonymous approver answers `422`; a non-owner or a project without
a roster-backed owner answers `403`.

## Tests

```bash
go vet ./...
go test ./...
```

The suite runs offline. It covers the six fixture states rule by rule, the
boundaries of the calendar rules, deterministic ordering, the update gate and its
refusals, optimistic versions and drifted fields, the idempotency key, replay,
path-traversal guards, the grounding gate, the agent's event protocol, the
scripted model's determinism, and every HTTP route including the approval
refusals and the SSE order.

## Known gaps

Honest list, with the reasoning, in [docs/missing-work.md](docs/missing-work.md):
single-process file locking, no authentication (this is a single-user local app),
JSON files instead of a real tracker API, and a scripted model that proposes only
the one change it can justify from the roster.

## License

Apache 2.0. See [LICENSE](LICENSE).
