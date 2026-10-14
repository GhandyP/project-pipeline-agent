# Missing work

What this repository does not do yet, why, and what the next step would be.
Written to be read before trusting the demo with anything that matters.

## Correctness boundaries

1. **The store locks per process, not per file.** `internal/tracker` serialises
   access with an in-process mutex per project and writes atomically through a
   rename. Two processes sharing one data directory can still lose a write.
   *Next step:* a file lock around the read-check-write, or move the store to
   SQLite where the transaction is the lock.

2. **Applying an update and appending the audit record are two operations.** A
   crash between the project rename and the audit append leaves the new version
   without its trail entry.
   *Next step:* one append-only log as the source of truth, with the project file
   derived from it, so either both facts land or neither does.

3. **Audit records are individually digest-ed, not chained.** Editing a record is
   detectable; deleting one and renumbering the rest is not.
   *Next step:* each record carries the previous record's digest.

4. **The session is in memory.** Restarting the process loses the ADK
   conversation history for a project. The durable artifacts are the proposal and
   the audit trail; a review itself is streamed and not stored.
   *Next step:* a session service backed by the store, and persisting the review
   next to the proposal it produced.

5. **A change set for one milestone is decided as a unit.** The gate applies
   every admissible change of a milestone together and then refuses the whole set
   if the milestone it would leave behind contradicts itself. That keeps a status
   with its reason or its progress, and it means a valid change is lost when it
   travels with an invalid one for the same milestone. The refusal names every
   change in the set, so the next attempt can be exact.

## Security and operations

6. **No authentication.** This is a single-user local app: it binds to
   `127.0.0.1` by default and anyone who can reach the port can read every
   project. Approving requires the name of a member of that project's team, which
   stops an accidental or arbitrary approval, but nothing proves who typed it: the
   name is accountability, not identity.
   *Next step:* an identity provider in front, and a per-project role check
   before the decision endpoint.

7. **No transport security.** `serve` speaks plain HTTP.
   *Next step:* terminate TLS in front, or serve HTTPS directly.

8. **No rate limiting or spend control on the live model.** A runaway loop is
   bounded at 24 tool calls per review, and nothing else bounds cost.
   *Next step:* a per-run token and call budget with a visible counter.

9. **Windows is untested.** Atomic rename semantics differ enough that the store
   should be proven there or refused.

## Product scope

10. **The tracker is a JSON file store, not a real tracker.** It is the adapter a
    real integration would replace: `internal/ports.Tracker` is the seam, and
    `Apply` is the only method that mutates.
    *Next step:* an HTTP adapter against a real tracker, with the same version
    check and idempotency ledger.

11. **The scripted model proposes one kind of change.** It assigns an owner from
    the roster, because that is the only update it can justify from the snapshot
    alone. It exists to make the loop deterministic and offline, not to model a
    planner.
    *Next step:* a scripted scenario per rule, including a refused one, so the
    offline demo also shows the gate saying no.

12. **Policy is barely configurable.** Two SLA numbers come from the environment;
    severity thresholds are constants in `internal/domain/rules.go`.
    *Next step:* a policy file per project, validated at load.

13. **The UI is not tested in a browser.** Its behaviour is covered by handler
    tests and by the SSE protocol tests, not by a real page load.
    *Next step:* one browser smoke test over the offline server.

14. **No i18n and no theme switch.** The UI is English and follows the operating
    system's colour scheme.
