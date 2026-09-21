# Adversarial critique — FERRY CLI plan (`docs/loops/cli/PLAN.md`, revision 1)

Reviewed against base `8c8abb9`, the base the plan names. Every `file:line` below
was opened and read; where the plan's citation and the file disagree I say so.
The plan's own citations are mostly accurate to within a few lines — that is
noise and is not listed. What is listed is where a citation supports a weaker
claim than the plan makes, or where the plan's design does not follow from the
fact it cites.

**A note on the base.** While this review ran, `5d17534` ("fix(api): correct
three defects the CLI planning pass found") landed on `main`, touching
`docs/api/{AGENTS.md,errors.md,openapi.yaml}`, `lib/ferry/api/errors.rb`,
`lib/tasks/ferry_docs.rake` and two specs. It fixes §1.4 items 1, 2, 4, 5 and
6 (`DESTINATION_TOO_NEW` flips to `retriable: false`; the two
`STEP_UP_REQUIRED` descriptions are replaced; a "Codes you will not see"
section is added; the bootstrap paragraph is added; `limit` gains a default).
Line numbers in this critique are against `8c8abb9`, the plan's stated base,
via `git show`. The consequence for the plan is G1 below.

## Verdict

**Re-plan the money-path units before implementation; the rest can be
implemented with amendments.** I found **4 Blockers, 9 Majors, 7 Minors, and 4
Gaps.**

The plan's central claim — the key is fsynced to disk before the first byte
leaves, and resending a `pending` step under the same key is always safe —
holds for the single-process, unchanged-credential case, and the two named
fault points (AC44/AC45) do test the two windows around `http.Client.Do`. The
Blockers are not in that argument. They are in what surrounds it: what exit
code the process reports when it dies *after* sending, what `resume` does when
the profile's credential is no longer the one that sent, what `resume` does
with a plan a human declined at the prompt, and a decision-table row that tells
a caller "new key" for a response that can mean "your original command may
have executed".

## Blockers

### B1 — Exit 1 `cli_fault` is defined as "before the wire", and three post-send failures have no other home

**Plan claim:** §5.6: exit 1 is "The CLI stopped before sending: config, I/O,
lock, user abort, bug before the wire — money moved: no". §5.9: "the root
command installs a single top-level renderer and recovers panics into a
`cli_fault` document"; §4.1 U5: `root.go` owns "signal handling". C3: "no
exception maps to refused" and lists deadline, transport error, unparseable
2xx and poll exhaustion as `pending`/6.

**Evidence:** Three failures occur after `Do` and are not on C3's list:

1. `SIGINT` during `Do` or during `runs.Record`. §5.13 says `root.go` maps
   "SIGINT → cli_fault document". A Ctrl-C while the execute request is on the
   wire is exit 1, "money moved: no".
2. `runs.Record` failing after the response arrived — `ENOSPC`, `EROFS`, an
   `fsync` `EIO`. §5.4 step 6 says a `Begin` failure is "exit 1, nothing
   sent"; step 8 (`Record`) has no failure clause at all.
3. A panic in the renderer after a 201 execute — "recovers panics into a
   `cli_fault` document", exit 1.

Server-side, every one of these may have executed: `Executor#send_and_record`
(`executor.rb:342–361`) has written a `reserved` command and sent
`create_transaction` before the caller sees any byte of the answer. A wrapper
that reads exit 1 as the plan defines it — "nothing sent, fix the local
problem, run again" — re-runs the command line, mints a new run (§7.2), and
sends a second transfer.

**Why it matters:** The plan's whole exit-code argument is that a wrapper can
trust the vocabulary. Exit 1 is the one code in the vocabulary that is
*defined* as safe to re-run, and it is emitted from paths that are not.

**Fix:** Make "has `Do` been called on a money step?" a single process-wide
flag set immediately before `api.Do` and cleared only by `Record` succeeding.
Every top-level exit path — signal handler, panic recovery, `Record` error,
`Open`/lock error on a resume — consults it: set → exit 6 with the run id and
"the request may have been sent; `ferry runs resume <id>`", never 1. Add a
fault point `during_record_after_send` and a `faultinject` case that sends
`SIGINT` while the fixture holds the request open; assert exit 6 and a
`pending` record for both. Also correct the read-class row "unknown code → 1":
a novel server code is not a CLI fault and exit 1's definition is false there
— use 3 or 7.

### B2 — `resume` under a rotated credential turns `IDEMPOTENCY_KEY_REUSED` into "retry with a new key"

**Plan claim:** §5.6: `409 IDEMPOTENCY_KEY_REUSED` → `refused_fix`/3 — "Refused;
nothing sent; nothing spent … retry with a **new** key". AC20 pins it. C2 says
the only way a different key is sent is a new run the caller started.

**Evidence:** `Ledger::Replay#refuse` (`replay.rb:103–118`) returns
`IDEMPOTENCY_KEY_REUSED` for two reasons: `different_request` (digest
mismatch, `:127–132`) and `different_credential` — the presenting key is not
the command's key and is not linked to it through the rotation chain
(`:138–142`). `keys create` (`api_keys_controller.rb:29`, body
`{name, environment, scopes, expires_at}`) has no "replaces" argument, so a
key minted by `ferry keys create --login` between a crash and a `resume` is
*not* linked, and the resume of the stored bytes under the stored key answers
`409 IDEMPOTENCY_KEY_REUSED` with `details.reason = "different_credential"`
and `Ferry-Command-Id` naming the original command (`executor.rb:220–224`,
`command_request.rb:432–439`).

That original command may be `completed`. The plan's row prints "nothing
spent; retry with a new key" for it, and `outcome.same_key_safe`/`next` in the
JSON document say the same. The table's input tuple (C4: operation, status,
code, headers, state, contradiction, transaction status) does not include
`details.reason`, so the table *cannot* tell the two apart.

**Why it matters:** This is the one response on the money path whose two
meanings are "you have a client bug, nothing of yours is under this key" and
"something of yours *is* under this key and you no longer have the credential
to read it". Mapping both to "new key" is a double-send instruction in the
second case, printed by the tool whose purpose is to never print one.

**Fix:** (1) On `resume`, before any request, compare the profile's
`token_prefix` (and `principal_id`) to the run record's; a mismatch is exit 7
with "re-login with the credential `ferry_sk_sandbox_grpQ0HNk…` that started
this run, then resume". (2) Add `details.reason` to the table's inputs: `REUSED`
+ `different_credential` on a money operation → `escalate`/7, never 3. (3) Add
the mutation "map `REUSED` regardless of reason" and a recorded scenario
`execute.key_reused.different_credential.409` (the recorder can mint a second
unlinked key and replay).

### B3 — `resume` can execute a plan the human declined at the prompt

**Plan claim:** §5.5: on a TTY without `--yes`, `--broadcast` asks `Execute?
[y/N]`; §5.3 Resume: "For a `--broadcast` run whose simulate is terminal and
whose execute is `not_started` with a `plan` recorded and unexpired, it starts
the execute step with the pre-minted key and the stored token." C8: "`--broadcast`
executes exactly the plan it displayed."

**Evidence:** §5.4 step 10 records the plan and token on the execute step
*before* the TTY confirmation. A human who reads the quote and presses Ctrl-C
(the ordinary way to say no when a prompt is up) leaves exactly the record
§5.3's resume rule matches: simulate terminal, execute `not_started`, plan
recorded, token stored, unexpired for up to five minutes. `ferry runs resume`
— run by the same human to "see what happened", by a wrapper on exit 1
(B1), or by the `runs list --pending` warning's own suggestion — sends the
execute with no prompt. Nothing in §5.3, §5.5 or AC48 says resume re-asks, and
the resume rule as written says it "starts the execute step".

**Why it matters:** The prompt is the consent step the design introduced for a
TTY; resume silently bypasses it inside the plan's TTL. The transfer is the
one the human saw, at the price they saw, which is why this is a consent
defect rather than a price defect — but it moves money a human had the chance
to refuse and did.

**Fix:** Record the prompt as a state on the execute step: `awaiting_confirmation`
written *before* the prompt is shown; `declined` (terminal, class 1, token
scrubbed) on `n`, EOF or a signal while waiting; `confirmed` on `y`, written
before `Begin`. `resume` executes only from `confirmed`; from
`awaiting_confirmation` it re-prompts on a TTY and exits 1 on a non-TTY
without `--yes`. Add a fault point `at_prompt` and a pty test: kill at the
prompt, resume with stdin closed, assert zero execute requests.

### B4 — A simulate that answers `202` yields a plan nobody can execute, and the plan's polling terminal rule reads a field the body does not have

**Plan claim:** §5.6: `202 (fresh or replayed)` → `pending` → §5.7; §5.6
terminal states: "`completed` → classify `result.body.status` as a 201 (0 or
8)". §5.4 step 10: "`--broadcast`: on simulate 201, record `plan{id, expires_at}`
and the token". Scenario `simulate.202` is in the recorded set. AC50 asserts
`completed` "classifies by `result.body.status`".

**Evidence:** `Commands::Simulator::Unknown` renders `202`
(`command_request.rb:263`, `:394–401`). When recovery later completes that
quote command, `Ledger::CompleteQuote#stored_body` (`complete_quote.rb:238–249`)
stores `{object: "simulation", command_id, quote, plan: {object, id, expires_at}}`
— no `token`, and there is no top-level `status`. `GET /v1/commands/{id}` for
that command returns `result.body` = that stored body (`command.rb:210–214`).
The token was handed to nobody: `render_simulated` (`command_request.rb:305–309`)
is the only writer of `plan.token`, and it never runs for a command that
completed asynchronously. `SIMULATE_REPLAY_REMEDIATION` (`:107–110`) says so
for the replay case; the 202 case is the same fact with no sentence.

So for a simulate step: `202` → poll → `completed` → `result.body.status` is
absent → the plan's rule has no branch, and the honest outcome is "a plan
exists that cannot be executed; re-simulate under a new key". Under
`--broadcast` the same path leaves the execute step `not_started` with `plan`
recorded from `result.body.plan` and **no token**; §5.3's resume rule then
"starts the execute step … with the stored token", i.e. sends
`{"plan_token": null}` → `404 PLAN_NOT_FOUND` (`executor.rb:240–241`) → exit
4. Safe by accident, not by design, and the `simulate.202` scenario the plan
lists would be the first test to hit it.

**Why it matters:** The plan's terminal classification is written for the
transaction body and applied to both operations. The simulate body is the one
the plan itself measured (§1.2.9) and it has no `status`. Every `--broadcast`
run that hits the 202 path has an execute step that can never legitimately
start, and the plan does not say so anywhere.

**Fix:** Split §5.6's terminal rule by operation: for `transfers_simulate`,
`completed` → `refused_resimulate`/4 with "FERRY completed this simulation
after the request ended; the plan token was never issued — simulate again
under a new key". In `--broadcast`, a simulate step that leaves via 202 marks
the execute step `unreachable` (terminal) at the moment the 202 is recorded,
so no `plan`/token is ever written to it. Give AC50 a simulate row and a
mutation ("apply the transaction rule to a simulate body" → wrong class).

## Majors

### M1 — Eleven acceptance criteria have no mutation, including a money-safety one; §3's claim that each has one is false

**Plan claim:** §3 preamble: "Each has a named mutation in §6.2 that must make
it fail; a mutation that cannot fail the criterion is a defect in the
criterion."

**Evidence:** Mapping §6.2's 62 rows to the 68 criteria leaves **AC1, AC2, AC3,
AC4, AC5, AC53, AC62, AC63, AC65, AC67, AC68** with none. AC53 —
"`IDEMPOTENCY_KEY_IN_PROGRESS` polls `details.command_id` and never resends
under a new key" — defends C2 and is the same class of hazard as M52
(`UPSTREAM_BUSY`-with-command), which does have one. AC2 (byte-stable
recordings) is the control that keeps AC4's drift detector switched on and has
none. AC62 is the e2e proof of C1/C2 and has none.

**Fix:** Add rows: AC53 "resend under a fresh key on `IN_PROGRESS`" → key
inequality; AC2 "drop the `request_id` substitution" → second recording
differs; AC4 "write to `tmp/` instead" → committed directory unchanged and
`git diff` clean where it should be dirty; AC62 "mint a new key on e2e resume"
→ two `Ferry-Command-Id`s; AC1 "remove a scenario from the manifest only" →
set inequality. AC5, AC65, AC67, AC68 can be declared non-controls explicitly,
which the learnings ask for ("Declare a control that has become vacuous").

### M2 — AC62's assertion cannot be evaluated at the fault point it names

**Plan claim:** AC62: "the fault binary dies **after the execute record is
written**; `runs resume` completes; the resume response carries either
`Idempotency-Replayed: true` or the same `Ferry-Command-Id` — proof that one
command exists for the key."

**Evidence:** "After the execute record is written" is the `after_record_written`
point of AC44 — before `Do`. Nothing was sent, so the resume request is the
first and only request: the answer is a fresh `201` with no
`Idempotency-Replayed` (`render_accepted`, `command_request.rb:275–280`, sets
no replay header) and a `Ferry-Command-Id` with nothing to compare against.
The "either" is unsatisfiable on the first branch and vacuous on the second.
Only the `after_send_before_record` point produces the replay the AC wants to
see. This is the "vacuous control" shape the learnings record: the property
is right and the named path cannot exhibit it.

**Fix:** Run AC62 at both points. At `after_record_written` assert: exactly one
request, fresh 201, run moves to terminal. At `after_send_before_record`
assert `Idempotency-Replayed: true` and equal `Ferry-Command-Id` across the two
requests (the fixture/real server saw the key twice). The mutation for each is
M3.

### M3 — Wave 1 and wave 2 are not orderable as claimed; U4's AC41 depends on U5's `root.go`

**Plan claim:** §4.2: wave 1 = U0, U1, U2 "disjoint files and languages …
build to them without talking"; wave 2 = U3, U4 "disjoint from each other";
U4 delivers AC41 (one JSON document on every exit path including usage errors
and CLI faults).

**Evidence:**

* §5.12 pin 2: U2's typed bodies "are decoded from the recorded 2xx bodies" and
  a U2 test asserts every recorded key is a struct field or in `Passthrough`.
  Those recordings are U0's wave-1 output. U2 cannot finish its tests until U0
  has committed. Same wave, real dependency.
* AC35 (U4) calls `GET /v1/me` and stores only on 200; AC37/AC39/AC40 need a
  server answering recorded bodies. C13 forbids a hand-written response, so
  the server is U3's fixture — same wave as U4.
* AC41 (U4) asserts one JSON document for "a usage error, a CLI fault" and that
  `outcome.exit_code` equals the process exit. §5.9 and §4.1 put that
  guarantee in `internal/cli/root.go`, owned by U5, wave 3. U4 owns no file
  that can implement what AC41 asserts.
* U1 (wave 1) owns `internal/harness/` with `Run(t, args, …) → (stdout,
  stderr, exit)`. Running `args` needs a root command; the root is U5's. U1
  can build a harness that takes a `*cobra.Command`, but the plan does not
  say so, and U4 uses it against noun packages that "do not own main.go".

**Fix:** Either move `root.go`'s JSON-document and exit plumbing into a wave-1
package U1 owns (`internal/cli/exit`), so U4 can test AC41 against it, or move
AC41 to U5. State that `harness.Run` takes a constructed `*cobra.Command`.
Re-draw waves as: 1 = U0 ∥ U1 ∥ U2-without-body-pins; 1b = U2 body pins + U3
(after U0); 2 = U4 (after U3); 3 = U5. Add the ownership lint the corridors
critique asked for (B6 there) so a missing/duplicate path fails before waves
begin.

### M4 — The fixture's sha256 match presumes the Go client and the Ruby recorder serialise identically, and the plan forbids the canonicalisation that would make it so

**Plan claim:** AC30: "A money `POST` matches on method, path, presence of
`Idempotency-Key` and body sha256; a request whose bytes differ from the
recording is unrecorded." C5: the CLI "rewrites no byte the caller supplied".
§4.2: U0 and U2 "build to [the format] without talking".

**Evidence:** The recorder drives `ActionDispatch::Integration::Session` with
Ruby hashes serialised by `JSON.generate` (insertion-ordered keys, no
whitespace). The Go flag builder (§5.1) emits its own serialisation —
`encoding/json` sorts map keys, orders struct fields by declaration, and
neither is `JSON.generate`'s order. For every flag-built body, the sha the
recorder wrote and the sha the client sends agree only if the two serialisers
happen to coincide. When they do not, the first U5 test that hits the fixture
gets `599 X-Fixture-Unrecorded`, and the cheapest fixes are exactly the two
the plan forbids: canonicalise before sending (C5) or match on path alone
(M32's mutation becoming the implementation). The placeholder problem
compounds it: the recorded execute body is `{"plan_token": "ferry_plan_PLACEHOLDER"}`
— its `body_sha256` is either of the placeholder (so the client must forward
the fixture's placeholder token literally) or of the real token (so the client
can never match). §5.10 does not say which.

**Fix:** Specify the wire form once, in the plan, for both sides: the recorder
writes the request body bytes *as the CLI will send them*, produced by a
Ruby serialiser pinned to the Go one (sorted keys, no whitespace — the shape
`encoding/json` gives a `map[string]any`), and the recorder asserts its bodies
are in that form. `--body` (verbatim) tests feed the recorded bytes as input.
State that `body_sha256` is over the placeholder-substituted body and that the
fixture hands out the placeholder token. Add an AC that the Go client's
flag-built body for the canonical simulate request equals the recorded bytes.

### M5 — The recorder has no assertion that a scenario recorded what its name says

**Plan claim:** AC1–AC4 (manifest equality, byte stability, envelope key set,
no writes elsewhere). §7.2: "A recorder that records the wrong thing … the e2e
job is the compensating control … **Partially closed**."

**Evidence:** None of AC1–AC4 asserts that `execute.policy_denied.403` recorded
a `403` with a `POLICY_*` code, that `execute.upstream_busy.with_command.503`
recorded a `Ferry-Command-Id`, or that `simulate.replay.201` recorded no
`plan.token`. A recorder that drove the wrong setup and captured a `403
WRONG_TOKEN_CLASS` under the policy-denied name passes every U0 criterion.
Downstream, only a U4/U5 test that asserts an *exit code* would notice, and
only for scenarios such a test consumes; AC20's table tests are pure and
consume no recordings. This is the tautology the learnings name — the
recording is both fixture and expectation — with the one break (the Go tests'
expected outcomes come from §5.6) not applied on the Ruby side at all.

**Fix:** `scenarios.rb` declares, per scenario, `expect: {status:, code:,
headers_present: [...], headers_absent: [...], body_has: [...], body_lacks:
[...]}` and the recorder asserts each against what the Rack app returned
before writing. The manifest carries the expectation so the Go loader can
assert the file still matches. The mutation is "record the wrong scenario
under a name" → the recorder spec goes red.

### M6 — `execute.destination_too_new.403` cannot be recorded through the Rack app, and CS-7 does not list it

**Plan claim:** §1.2.7 and §1.4.1 make `DESTINATION_TOO_NEW` the plan's
headline documentation defect; §5.10 lists `execute.destination_too_new.403`
as a recorded scenario; CS-7 names only `upstream_busy.with_command` and
`replay_expired` as uncertain.

**Evidence:** `SpendPolicy::Evaluate`'s check (`evaluate.rb:295–301`) fires only
when `context.destination_usable_after` is non-nil. Its only production caller,
`Ledger::Begin` (`begin.rb:207`), passes `destination_usable_after: nil`; the
comment at `evaluate.rb:292–294` says the mirror that would supply it "does
not exist yet". The code is unreachable from any HTTP request. Recording it
means stubbing `Begin` or `Evaluate` inside the Rack app — which produces a
body "out of the Rack app" in letter and a hand-driven fiction in spirit, the
second authority C13 exists to forbid. This is the plan's own defect class 4
(unreachable rule), on the code it put in bold.

**Fix:** Move the scenario to CS-7's "unreachable" list now, with the reason,
and keep the table row (AC19 still requires it). Correct §1.4.1: the code is
unreachable today, and its `retriable` flag is being corrected in the working
tree (G1). The Go row should be asserted from the table alone, as CS-7's
degradation clause already allows — say so rather than discover it.

### M7 — `flock` on a file that is replaced by rename is not a lock after the first rewrite

**Plan claim:** C12 / AC12 / M5: concurrent invocations on one run "take an
exclusive non-blocking `flock` and the loser stops rather than racing";
`runs.Record` and the execute-step `Begin` rewrite `<run_id>.json` by
temp+fsync+rename.

**Evidence:** `flock(2)` locks an open file description's inode. `rename(2)`
replaces the directory entry with the temp file's inode. After process A's
first `Record`, A holds a lock on the *old* inode; process B opening
`<run_id>.json` opens the *new* inode and its `LOCK_EX|LOCK_NB` succeeds. AC12
passes (B opens before A's first rewrite) while the lock is ineffective in
every window after it — which for a `--broadcast` run is the whole execute
step. The plan says the lock is "tidiness over the record file; the API is the
correctness argument" — true for double-*sends*, but two writers rewriting one
record via rename also lose each other's updates, so the record a later
`resume` reads may be the stale one.

**Fix:** Lock a sidecar `<run_id>.lock` that is created once and never renamed;
hold it from `Open` through the last `Record`. Re-cut AC12 so the second opener
arrives *after* the first has rewritten the record once. `Scrub`-on-load
(`runs list`) must take the same lock or skip locked runs.

### M8 — The `--broadcast` inter-step window the token is stored for has no fault point

**Plan claim:** §5.3 "Why the plan token is stored at all": to survive "a crash
in the window between the simulate 201 and the execute `Begin`". AC44/AC45
name `after_record_written` and `after_send_before_record`.

**Evidence:** Both named points are on a single step. The window that
justifies putting a bearer secret on disk — after the simulate `Record` with
the token written onto the execute step, before the execute `Begin` — is not a
fault point and has no test that resume picks up the stored token and executes
under the pre-minted key. The other adjacent window — simulate `Do` returned
but simulate `Record` did not — loses the token forever (the replay has none,
`command_request.rb:325–333`), and the plan's resume rule does not name it.

**Fix:** Add `after_simulate_recorded_before_execute_begin` and
`after_simulate_send_before_record` fault points. Assert, respectively: resume
executes with the stored token and the pre-minted execute key (fixture saw
exactly one simulate and one execute, keys `<run>-simulate`/`<run>-execute`);
resume gets the replayed 201 without a token and exits 4 having sent no
execute.

### M9 — "Documentation is the control" for wrapper re-runs is not acceptable when a cheap code control exists

**Plan claim:** §7.2: a wrapper that re-runs the command line on exit 6 or 7
sends money twice; mitigations are `--idempotency-key` and a stderr warning
from `runs list --pending`; "**Not closed**; the documentation is the control."

**Evidence:** The `until ferry transfers create …; do sleep 5; done` loop the
plan describes leaves a `pending` record after iteration 1 in the same
`$FERRY_HOME`, against the same profile and environment. The CLI *knows* on
iteration 2 that an unresolved money run exists for this credential. It
prints a warning and proceeds. Both `outcome.same_key_safe` and the `next`
sentence are advice a `set -e` script never reads.

**Fix:** Default fail-closed: a money command refuses (exit 1, "unresolved
run 01J… exists for this profile; `ferry runs resume 01J…` or pass
`--allow-pending`") when any run against the same profile+environment is
`pending` or `answered` with class 5/6. `--allow-pending` opts out for callers
who genuinely run concurrent transfers. This converts the described
double-send into a refusal in exactly the wrapper shape §7.2 names, and makes
`runs resume` the path of least resistance. Add the mutation "proceed when a
pending run exists" → request count 1.

## Minors

### N1 — `IDEMPOTENCY_KEY_REPLAY_EXPIRED` carries `details.state`, which the table discards

`Replay#refuse` returns `{"expired_at", "state"}` (`replay.rb:114–117`), and
`replay_expires_at` is set at `Begin` (`begin.rb:147`), seven days from
creation, so a command can expire in `needs_operator` or `upstream_unknown`
as well as `completed`. Mapping every case to `escalate`/7 is safe; rendering
`details.state` ("the command under this key is `completed`") costs nothing and
is what the human escalated to needs first. CS-7's note that the scenario
"needs a `completed` row aged past 7 days — builders can set
`state_changed_at`" names the wrong column: it is `replay_expires_at`, and
`spec/requests/api/v1/command_request_spec.rb:291–304` already has
`expire_replay_window!`.

### N2 — AC13's scrub is a local expiry decision the plan elsewhere refuses to make

§10.2: "A local 'is the plan expired?' refusal — the server's clock is the
authority." AC13 scrubs the token when `plan.expires_at + 60s < now` by the
local clock. A client clock more than 60 s ahead scrubs a live plan from a
`not_started` execute step, and resume then exits 4 for a plan the server
would still honour. Also, "Scrub runs on every `runs` load" means a token on a
machine where nobody runs `ferry runs` is scrubbed never; the bound "five
minutes plus grace" holds only while the process runs. State both honestly or
scrub on the execute step's terminal write only, and treat expiry as the
server's answer.

### N3 — The plan's preamble names the wrong criteria for its headline proof

Line 26 and C1: "AC42 and AC43 kill the process … AC42 kills the process to
prove it." In §3, AC42 is the render secret sweep and AC43 the pre-check; the
kill tests are AC44/AC45, and §3.5 has a parenthetical apologising for the
numbering. Fix the four references; AC66/AC68 audit by number.

### N4 — §1.2.13 overclaims: execute does not refuse unknown body keys

"Unknown body keys are refused … on the transfer endpoints by the translator
(`quote_request.rb:247`, `256`)." That is the simulate translator.
`TransfersController#execute` (`transfers_controller.rb:66–73`) passes
`request.request_parameters` straight to the digest and reads `plan_token`
from it; an extra key is digested and ignored. AC63 happens to test `--body`
on simulate, so it is not wrong, but the measurement is, and U7's
`DOC-DEFECTS.md` would repeat it.

### N5 — C13 is overclaimed against AC23/AC27, and `599` is a 5xx

AC23 (hang, refuse, truncate) and AC27 (retry on 5xx, `control_create` never
retried) need `httptest` servers that answer synthetic 500s and half-closed
bodies — hand-written FERRY responses by any reading of C13. Say "no Go test
asserts against a hand-written FERRY *body*; transport faults are synthesised".
Separately: the fixture's `599` for an unrecorded request is a `5xx`, and the
read and `control_revoke` retry policies retry `5xx` within a 60 s budget, so
every unrecorded-request test either uses the fake clock or waits a minute.
Exempt `599` from retry explicitly.

### N6 — M48's stated detector is "the test times out"

A control that fails by hanging the suite is a control nobody re-runs. On a
non-TTY the prompt read hits EOF immediately; assert "no prompt text on
stderr and execute sent" rather than a timeout.

### N7 — `golang.org/x/sys` is not needed for `flock`

`syscall.Flock` with `syscall.LOCK_EX|syscall.LOCK_NB` is in the standard
library on darwin and linux. One fewer dependency on a money tool's supply
chain, which §5.13 says is part of its threat model.

## Gaps

### G1 — §1.4 and U7's deliverable are already stale against `main`

Five of §1.4's six defects (items 1, 2, 4, 5, 6) are fixed in `5d17534`,
committed while this review ran. `DESTINATION_TOO_NEW` is `retriable: false`
there, with a comment giving the plan's own reason; `errors.md` gains a "Codes
you will not see" section that also declares `DESTINATION_TOO_NEW` unreachable
(M6). The plan's base is now one commit behind `main`, U7's `DOC-DEFECTS.md`
would file fixed defects, and AC67's line numbers (`AGENTS.md:361`) have
moved by the fifteen lines the bootstrap paragraph added. Re-base the plan to
`5d17534`, drop the fixed items from §1.4, and have U7 own a *check* that each
remaining filed defect is still present rather than a static list.

### G2 — Only one `--idempotency-key` for a two-step `--broadcast` run

§5.3: "A caller-supplied `--idempotency-key` replaces the step's key." A
`--broadcast` run has two steps that need two distinct keys, and §5.1 offers
one flag. Say what it names (the execute key, with the simulate key derived
`<K>-simulate`? both refused?) and what `FindByKey` does with it.

### G3 — The recorded `response` in a simulate-only run record

§5.3: `runs.Record(step, response, outcome)` fills `response`. AC46: a
simulate-only run "stores no token in the run record". The fresh simulate 201
body *is* the token's only carrier. `Record` must strip `plan.token` before
writing (and M45 catches only the deliberate store). State it; otherwise the
natural implementation violates C6 on every simulate.

### G4 — Unparseable 5xx and unlisted statuses on a money request

AC17 returns `ErrEnvelopeShape` for a body without the seven keys. A proxy 502
with an HTML body, a `415`, a `413` — none has a §5.6 row for the money class.
C3's conservative default (`pending`/6) is right; write the row so the
coverage test has something to assert rather than falling through.

## The exit-code vocabulary, mapped

Totality: every catalogued code reaches a row via the `400 *`/`401 *`/`422 *`
wildcards or the unknown-code default, so the partition is total over what the
endpoints emit. It is padded — `WEBHOOK_*`, `CURSOR_INVALID` and
`UPSTREAM_CREDENTIALS_INVALID` get money-class rows nothing can trigger — and
the wildcards mean AC19's "every code mapped" is satisfied by rows that never
name the code, which is weaker than it reads.

Disjointness: `EXECUTION_SUSPENDED` and `UPSTREAM_CONTRACT_VIOLATION` sit in 3
on simulate and 4 on execute and are disjoint because operation is an input.
`UPSTREAM_BUSY` is 5/6 by header and is disjoint. **`IDEMPOTENCY_KEY_REUSED` is
in two buckets (B2)** because the discriminant, `details.reason`, is not an
input. `PLAN_KEY_MISMATCH` → 4 says "the plan is spent" while `Begin`'s
`Refused` rolls the consumption back (`begin.rb:47–51`, `:391–396`); the
action (re-simulate with the right key) is right, the sentence is not.

On the `type: object` question: verified — `/v1/me` 200, `POST /v1/api_keys`
201, simulate 201 and execute 201 are `schema: { type: object }`
(`openapi.yaml:69`, `:116`, `:237`, `:316` at base). The plan's diagnosis is
right and its remedy is half right. Hand-writing nine routes is fine; making
`cli/testdata/recorded/` the *type authority* for those bodies builds a second
contract only the CLI can read, and §10.1 row 8 waits for schemas nobody in
this plan writes. The contract should be fixed: add `Simulation`, `Transaction`,
`Principal` and `ApiKey` schemas to `openapi.yaml`, pin them in
`spec/docs/api_docs_spec.rb` against the serialisers (the repo already pins
`TransferRequest`, `Command.state` and the api-key bodies that way,
`api_docs_spec.rb:256`, `:366`, `:426`), and let AC25-style Go pins read the
schema. Recordings then test behaviour, not shape. This is a small Rails-side
unit and belongs in wave 1.

## What the plan gets right

Verified against the code, and worth not churning:

* **The preflight order and its consequence.** `Executor#preflight`
  (`executor.rb:193–201`) is existing key → `gateway.available?` → plan, so a
  replayed key replays even when the deployment answers
  `UPSTREAM_NOT_CONFIGURED`, and a fresh execute never touches the plan. The
  plan's "nothing spent" for that row is correct.
* **`Denied` commits, `Refused` rolls back, and the tell is `Ferry-Command-Id`.**
  `begin.rb:45–62`; `refuse_denied` sets the header (`command_request.rb:416–424`),
  `refuse` sets it only from `details.command_id` (`:432–439`). Mapping the
  policy verdicts and `DESTINATION_TOO_NEW` to `refused_resimulate` is right,
  and the `DESTINATION_TOO_NEW` retriability finding was correct enough that it
  is being fixed as this is written.
* **`UPSTREAM_BUSY` split by header.** `read_quote` returns a command-less
  `Refused` (`executor.rb:289–294`); `give_up` returns `Retriable` with a
  `failed_retriable` command and schedules recovery (`:467–481`), which
  replays as `202` (`replay.rb:71–72`). The 5/6 split and the "resend same key
  → 202 → poll" sequence are exactly what the code does.
* **Never retrying `POST /v1/api_keys`.** The `IdempotencyKey` parameter is
  declared on exactly the two transfer operations (`openapi.yaml:218`, `:297`;
  pinned at `api_docs_spec.rb:408`). A retried key mint is an unowned
  credential. C14's exception is right and AC38's remediation is the correct
  one.
* **`REPLAY_EXPIRED` → escalate, overriding the server's remediation.** The
  server says "retry with a new key" (`command_request.rb:97–101`); the plan
  refuses to automate that for a money key whose outcome the CLI may still hold
  locally. Conservative in the right direction.
* **Contradiction dominates state; 201 is not success; `pending` is
  deliberately ambiguous.** All three follow the ledger's own semantics
  (`command.rb:190–206`; `Ledger::Complete` accepting any correlated status),
  and the render rules (no "success", no "✓", "not settled") are the right
  posture for acceptance-not-settlement.
* **The key scheme.** `<ULID>-simulate`/`-execute` is printable ASCII under 255
  bytes (`command_request.rb:89–90`), unique across processes without
  coordination, and refuses argument-derived keys for the right reason (§10.2).
* **Storing the exact body bytes and their digest.** `RequestDigest` is over
  the body (`command_request.rb:126–128`); a resume that re-marshals would be
  `IDEMPOTENCY_KEY_REUSED`. Recording bytes, not a struct, is the only correct
  choice and the plan makes it.
* **The fixture-backend facts.** `Fixture#available?` (`fixture.rb:181–183`),
  `SELECTION` (`backend.rb:38–45`), `Organizations::Provision` creating
  unsuspended NULL-cap policy rows on a database with no open restore
  (`provision.rb:75–80`, `:94–96`), the PAT rake fence (`ferry_pat.rake:27–40`)
  and the token on its own stdout line (`:158–170`) — every bootstrap step the
  e2e job depends on was measured correctly.
* **`errors.md` is generated from `CATALOG` and pinned both directions**
  (`ferry_docs.rake:17–20`; `api_docs_spec.rb:436–449`), so AC19 reading the
  document is reading the catalogue at one remove, with a red Ruby spec in
  between if they diverge. The chain is sound.
* **The 14-key command body and the poll intervals** (`command.rb:190–206`,
  `command_serializer.rb:28–43`) and CS-13 (`poll` is a path, `:199`).
* **`--output` never inferred from the TTY**, credentials in a 0600 file for
  a headless consumer, and the honest §1.3 table of what works today.

## The plan's weakest assumption

**That the Rails-recorded interactions can be the fixture's authority for
request bytes as well as response bodies** — AC30's sha256 match plus §4.2's
"build to the format without talking" plus C13's ban on hand-written
responses. It is the assumption most likely to be false on first contact
(M4), and the reachability half of it is already false for one scenario (M6)
and admitted uncertain for two more (CS-7).

What it costs if false: U3 and U5 cannot go green against the fixture without
either loosening the match (path-only, which is M32's mutation shipped as
code) or canonicalising the body (C5 broken), or hand-writing recordings (C13
broken). Whichever is chosen, the money-path behaviour tests (AC44–AC59)
degrade to "the client did *something* on this path" and the only proof that
the client sends what the server expects becomes AC61/AC62 — one happy path
and one crash-resume in a 20-minute CI job. The decision table (AC19–AC24)
survives intact; it is the table's *connection to the wire* that is lost.

Fixing the serialisation contract in the plan (M4), adding recorder
self-assertions (M5), and moving the four response schemas into `openapi.yaml`
so recordings carry behaviour rather than shape, are the three changes that
make the assumption cheap to be wrong about.

## Could not evaluate

I did not run the Rails suite, build Go, or probe a server. I did not verify
`goreleaser`/Homebrew behaviour (CS-8, CS-10) or `x/term` pty behaviour on
macOS (CS-12). I did not measure whether `spec/cli/recorded_interactions_spec.rb`
can produce byte-stable output under the suite's random ordering and
`truncation.rb`'s fleet-wide cleanup; the plan's §4.3 item 5 flags the risk
and I have no evidence either way.

# Verification of PLAN v2

Reviewed at `f476389` (`docs/cli-plan-v2`), a narrow pass on the
reconciliation only. Every `file:line` below was opened; PLAN citations are
against the v2 file. Settled design is not re-argued.

**Verdict: start implementing, except U8 and U5 until two one-paragraph
amendments land (V1, V2 below).** No Blocker. **2 Majors, 7 Minors, 2 Gaps.**

## The four blockers

| | Status | Where it lives now | Held? |
| --- | --- | --- | --- |
| B1 | **Narrowed** | C17 (PLAN:324–329), §5.4 steps 10–11 (:1103–1107), AC69 (:679–686), M73–M76 | Cases 1 and 2 (SIGINT on the wire, `Record` failure) are closed by the flag. Case 3 — a fault *after* `Record` succeeded — is reopened by the flag's clear-point. See V1. |
| B2 | **Closed** | C19 (:336–341), §5.3 resume step 2 (:1041–1043), AC70/AC71, §5.6 `REUSED` rows (:1181–1182), M77/M78 | Both halves of the fix landed: prefix+principal compared before any request; `details.reason` is a table input with absent → 7. `Replay#refuse` (`replay.rb:103–110`) does emit the reason; `Executor` merges `command_id` (`executor.rb:220–224`). Non-vacuous. |
| B3 | **Closed** (stricter than proposed) | C16 (:316–323), state diagram (:1003–1009), AC48/AC72/AC73, M79–M81, §10.1 row 13 | No `confirmed` state; `resume` re-asks or requires `--yes` for `pending` as well as `awaiting_confirmation`. Every send path is enumerated: `--yes`; non-TTY + `--broadcast`/`execute`; TTY + `y`. `--idempotency-key` same-body "resumes that run" (step 5) is not explicitly routed through the resume order, but the direct command carries its own consent on a non-TTY and prompts on a TTY, so no gap in the *property*. One coverage gap: V10. |
| B4 | **Closed** | C8 (:303–306), AC74/AC75, §5.6 terminal table (:1209–1218), step 12 (:1108–1111), `simulate.202.then_completed` (:1332–1334), M82/M83 | Rule split by `operation`; `unreachable` written when the 202 is recorded, so nothing ever reads a `null` token. `stored_body` (`complete_quote.rb:238–249`) confirms no `token`/`status`. M82 is non-vacuous because the transaction rule maps an absent `status` to 0. |

## Findings

### Majors

**V1 — C17's flag is cleared too early; AC69(c) and §5.4 step 11 contradict each other.** C17 (PLAN:325–326) and step 11 (:1105–1107) clear the flag "only when `Record` returned nil"; §5.9 (:1243–1246) has the signal handler and panic recovery choose 1 or 6 by that flag. Render is step 14, after the clear — so `render_panics_after_201` (AC69(c), :682) reaches the panic recovery with the flag *unset* and exits 1, while AC69(c) demands 6. M75 ("renderer panic → `cli_fault`", :1596) is the described behaviour, not a mutation of it. Same clear-point also makes Ctrl-C during `poll.Watch` (step 12, after a recorded 202) exit 1 — the most common human interruption of a 120 s poll, not in §7.1 (A3 is deadline, A17 is on-the-wire). C18 catches the wrapper re-run for the poll case (the step is `answered`/6) but not for the post-201 panic (the step is `answered`/0). *Fix:* make the flag monotonic for the process — set before the first money `Do`, never cleared — and have post-`Record` exits report 6 (or the recorded class); AC69(c) then holds as written. **B1: narrowed.**

**V2 — U8's `Simulation` pin omits `meta`, so AC85 and AC86 cannot both be green.** AC85 (:764–772) pins `Simulation` to `CompleteQuote#stored_body ∪ {plan.token}`. The replayed simulate 201 renders `result.body.merge("meta" => {"remediation" => …})` (`command_request.rb:329–331`), §5.6 requires the CLI to render `meta.remediation` (:1169), and AC86 (:545–549) pins the Go struct to the schema *both directions*. A Go `Meta` field reddens AC86; adding `meta` to the schema reddens AC85 as written. *Fix:* pin against `stored_body ∪ {plan.token, meta.remediation}`, both documented as conditional (fresh-only / replay-only).

### Minors

**V3 — Step order makes AC54's resume branch need `--allow-pending`.** §5.4 runs the orphan check (step 3, :1092) before `FindByKey` (step 5, :1094–1095). A caller passing `--idempotency-key K` with the same body to "resume that run" is by definition naming an unresolved run with a free lock — step 3 exits 1 first. *Fix:* exempt the run `FindByKey` names, or swap steps 3 and 5.

**V4 — A headless machine cannot decline an `awaiting_confirmation` orphan.** Only SIGKILL or an unhandled signal leaves the state (:1015–1017; SIGHUP — a closed terminal — is not in the `declined` list, though SIGINT/SIGTERM are). Non-TTY `resume` without `--yes` exits 1 and leaves it (AC72, :694–696); `--yes` would *send* it; `prune` removes only terminal records (AC57, :670–672). So every later money command on that `$FERRY_HOME` exits 1 until a human reaches a TTY, and `--allow-pending` on every call is the only escape. Fail-closed in the safe direction, so Minor. *Fix:* add SIGHUP to the declined signals; add `runs decline <id>` (writes `declined`, scrubs) or let `prune` take `awaiting_confirmation`.

**V5 — The sidecar lock's probe and prune are unspecified and reintroduce narrower versions of M7.** `Orphans` (AC90, :460–464) and `Scrub` (AC13, :445–449) decide "held by a live process" by acquiring `LOCK_NB`; `flock` has no test-without-acquire, so a concurrent legitimate `runs resume` can hit `ErrRunLocked` while another command's orphan probe holds the sidecar for a millisecond. `prune` removes records (:1083–1084) and presumably sidecars; a sidecar recreated after unlink is a new inode, so "created once and never renamed" (:972–973) does not hold across prune. *Fix:* prune takes the lock and deletes record then sidecar; resume retries `LOCK_NB` a few times before reporting `ErrRunLocked`.

**V6 — AC82/C6's "the only place a token may be written" is false by construction.** AC82 (:453–455) and C6 (:296–299) say the execute step's `plan_token` field is the token's only resting place. The execute step's `body` is `{"plan_token": "…"}` verbatim (C1, :269–271; AC55) and its `body_sha256` must not change on scrub (AC11, AC13 :447). The token therefore persists in `body` for the record's lifetime. Spent and expired, so low risk — but say so, and require `runs show` to redact `body.plan_token` (AC57).

**V7 — Two e2e ACs assert facts with no named observable.** AC63 "zero requests" (:750–751) and AC62's "`Ferry-Command-Id` equal to the first response's" (:746–748) — the fault binary died before recording the first response, and the real server keeps no request log the test reads. *Fix:* name the observable (`http == null` in the JSON document for AC63; the first id from the `--debug` trace on stderr, or `bin/rails runner` against `commands`, for AC62 — or drop to `Idempotency-Replayed: true`, which alone proves the property).

**V8 — U8 scope and pin mechanics.** (a) `List.data.items` is a seventh `{ type: object }` (`openapi.yaml:651`) and is the body `keys list` (AC39) renders; U8 names six (:1380–1382). (b) `ApiKey.token` is conditional (`api_key_serializer.rb:40`) and AC85 does not say how it is pinned, unlike `Simulation.plan.token`. (c) CS-14's fallback — "a rendered fixture object's key set" (:1743–1745) — cannot yield `Principal`'s nested `environment` and `user` shapes in one render: an API-key principal has `user: null`, a PAT has `environment: null` (`principal_serializer.rb:44–52`, `66–70`). One render per credential class, or a key-list constant. (d) CS-14 "U8 adds one without changing output" contradicts §4.1 U8 "Touches no serializer" (:857). `spec/docs/api_docs_spec.rb` has 32 examples (`rspec --dry-run`); U8 adds and edits none — achievable once (a)–(d) are stated.

**V9 — M80 can survive as worded.** "Write `awaiting_confirmation` *after* the prompt" (:1600) — if `at_prompt` fires while blocked on the read and the mutant writes after *displaying* the prompt, the state is already `awaiting_confirmation` and AC72's assertion passes. Pin the mutant as "write only on `y`", or define `at_prompt` as "after display, before the read".

### Gaps

**V10 — C16's third arm has no control.** `transfers execute --plan` on a TTY without `--yes` prompts (§5.5, :1141–1143). AC48 covers `--broadcast`, AC73 covers `resume`; nothing asserts the direct command writes `awaiting_confirmation`, prompts, and writes `declined` on `n`, and no mutation names it.

**V11 — CS-7's fallback covers AC20 but not AC52.** If `execute.upstream_busy.with_command.503` cannot be recorded, "the Go row is asserted from the table alone" (:1729–1731) keeps AC20 but leaves AC52's behavioural half (resend same key → recorded 202 → poll, :654–657) with no body C13 permits. A `failed_retriable` row built with the ledger builders replays as 202 (`replay.rb:71–72`), not as the 503, so the builder route does not reach it. Say what AC52 becomes. Also: `expire_replay_window!` is a private method of `command_request_spec.rb:339`, not `spec/support`; U0 "reads, does not edit" support files and must copy the one `UPDATE`.

## Mutations sampled

M5, M63–M68, M70–M75, M77–M83, M86–M92, M94–M96, M98–M100 read against their ACs. All are reachable in the example named and assert something the AC establishes, with three notes: M75 is the plan's own mechanism (V1), M80 is ambiguous (V9), and M69 mutates the *test* rather than the system — acceptable as a demonstration that AC62's first branch is non-vacuous, with M3 as the code-side mutation. M98 depends on the SIGINT racing the response; the `pending`-record assertion makes it deterministic in the failing direction. The count is 100 rows over 89 criteria minus AC68; checked.

## The two stated weak spots

**CS-15** (:1746–1749) names no fallback — "Blocks: AC69(a)". Not load-bearing enough to block a start: AC69(b) and (c) exercise the flag independently, and the natural fallback — the handler cancels the request context, `Do` returns through AC23's after-write branch, same exit 6 — is one sentence. Name it.

**CS-7** (:1722–1731). `replay_expired` and `different_credential` are low risk (the SQL exists; `KeyChain.linked?` is false for any unlinked key, `replay.rb:138–142`). `upstream_busy.with_command` is the real one and its fallback is incomplete (V11). None blocks a start; U0 is wave 1 and the amendment path exists.

## New states, units and waves — checked, no finding

`awaiting_confirmation`, `declined`, `unreachable` each have a writer and a reader (:1003–1009, :1046–1054); `declined`/`unreachable` are terminal and prunable; `awaiting_confirmation` is cleared by resume (V4 for the headless case). U2a/U2b edit `internal/api/` sequentially, not concurrently. `internal/runs/` (U1) and `internal/noun/runs/` (U5) are distinct prefixes. `harness.Run` taking a `*cobra.Command` (:814–816) removes the U1→U5 dependency M3 found. Waves 1/1b/2/3 are orderable as drawn. C18's orphan set (:332–334) excludes a `not_started` execute step holding a token (the `after_simulate_recorded_before_execute_begin` window); that run's execute was never sent, so a re-run is one transfer, not two — correct as drawn.

## Not evaluated

No Go was built and no signal was delivered; CS-11, CS-12, CS-15 remain assumptions. The Rails suite was not run beyond `rspec --dry-run` on `spec/docs/api_docs_spec.rb`.
