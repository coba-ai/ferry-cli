# FERRY CLI — implementation plan

**Slice:** `plan/cli` · **Revision 2.1** · **Base:** `f476389` (`main`; v1 was
planned at `8c8abb9`, v2 at `3239f06`)
**Language:** Go (fixed) · **Binary:** `ferry` (fixed) · **Distribution:**
Homebrew, tap `kurenn/tap` (fixed) · **Module:** `github.com/kurenn/ferry-cli` (A400)

This plan covers the first real consumer of the FERRY API: a single static Go
binary, `ferry`, that a human at a terminal or a program capturing stdout can
drive the API with. It is written to be handed to several implementing agents
in parallel; §4 assigns every file to exactly one unit and states what must not
run at the same time as what.

Revision 2 reconciles `docs/loops/cli/CRITIQUE.md` (4 Blockers, 9 Majors, 7
Minors, 4 Gaps); revision 2.1 folds in its "Verification of PLAN v2" (0
Blockers, 2 Majors, 7 Minors, 2 Gaps, cited as `V1`–`V11`). Every change is a
row in §11.3, keyed to the finding that drove it and the mutation that now
covers it. Findings the plan declines or alters are in §9 with the evidence
read for the decision. Findings are cited as `CRITIQUE B1` / `M1` / `N1` /
`G1` / `V1`; mutation rows in §6.2 are `row M1` and so on, so the two `M`
series do not collide.

Four things about this document before anything else.

**Every load-bearing fact was read in the code, not in the documentation.**
§1.2 is a numbered list of measurements with `file:line` citations, §1.4 is the
list of documentation defects this exercise found and their current status,
and where documentation and code disagree the code is what this plan builds
against.

**The money-safety problem of a CLI is where the idempotency key lives, and
what the process says when it dies.** A process can be killed between deciding
to send and learning the answer. §5.3 is the run ledger — the key is durable on
disk, fsynced, before the first byte leaves — §5.4 is the exact order of every
step on the money path, and §5.6 fixes what every exit path reports once a
request may have left (never "nothing sent"). AC44, AC45, AC69, AC72, AC80 kill
the process at named points and prove the next invocation finds the key and
does not send what nobody agreed to.

**Consent is given per invocation and is never stored.** An execute request
leaves the process only if this invocation's command line or this
invocation's TTY said yes (C16). Stored plan tokens, stored step states and
`runs resume` do not substitute for it.

**Live execution is impossible and the deployment has no upstream credential.**
Both are facts about the API (§1.3). The CLI is planned so that its happy path
is reachable — against a FERRY running the fixture backend, which is what CI
runs (§5.14) — and so that against today's deployment it answers `503
UPSTREAM_NOT_CONFIGURED` honestly.

---

## 1. The problem

FERRY's thesis is that a program is the first-class consumer. The API is
idempotent, simulate-first, and answers every failure with a stable code and a
decision table (`docs/api/AGENTS.md`, "What to do with each answer") that says
whether money moved and what to do next. No consumer has yet been built against
it, so nobody has had to encode that table as behaviour, hold an idempotency key
across a crash, or decide what a terminal should print when the API says
"accepted upstream, not settled".

A CLI makes those decisions once, for everyone. It also exposes the API's
sharp edges to a second audience: a shell script or agent capturing stdout
needs a machine-readable outcome and an exit code that distinguishes "refused,
nothing happened, change the request" from "unknown, do not retry, escalate".

The design problem is therefore not "wrap the endpoints". It is: **build a
client whose every failure path preserves the API's idempotency guarantee,
whose every exit code is true about whether a request may have left, and whose
output — in both modes — never claims more than the API supports.**

### 1.1 What is already built, and what this slice must not re-litigate

| Built | Where | What this slice inherits |
| --- | --- | --- |
| Idempotency and replay | `app/controllers/api/v1/concerns/command_request.rb`, `app/services/commands/ledger/replay.rb` | Same key → same outcome, never a second execution. The CLI's job is to never lose the key and never invent a second one. |
| The decision table | `docs/api/AGENTS.md`, verified against `app/services/commands/executor.rb` and `ledger/begin.rb` | Encoded as `internal/outcome` (§5.6). |
| Error envelope, seven keys always present | `lib/ferry/api/errors.rb:485–499` | Decode requires all seven; branch on `code`, and on `details.reason` for one code (§1.2.25). |
| Two credential classes | `TransfersController:48`, `ApiKeysController:27`, `CommandsController:32`, `CorridorsController:35`, `MeController:14` | One profile holds one credential per class (§5.2). |
| The fixture backend | `lib/ferry/oms/gateway/fixture.rb`, `lib/ferry/oms/backend.rb:38–45` | The only way the money path can be driven end to end today. CI runs it (§5.14). |
| Contract pinning on the Rails side | `spec/docs/api_docs_spec.rb` | Extended by U8 to the four response bodies the CLI reads (§5.12). |
| A prior CLI design | `../polygon-oms-research/PLAN-V2.md` §14.2 | cobra, run ids, dry-run-by-default. Adopted where the API supports it; §9 records where it does not. |

### 1.2 What we measured

Items 1–24 were read at `8c8abb9` and re-checked at `3239f06` where the file
changed; 25–30 were added in revision 2. Numbered so §5 and §7 can cite them.

1. **Credential class per endpoint is a controller declaration, enforced before
   the action and again in the policy.** `TransfersController` and
   `CommandsController` declare `accepts_principals :api_key`
   (`transfers_controller.rb:48`, `commands_controller.rb:32`);
   `ApiKeysController` declares `:personal_access_token`
   (`api_keys_controller.rb:27`); `CorridorsController` and `MeController`
   accept both (`corridors_controller.rb:35`, `me_controller.rb:14`). The wrong
   class is `403 WRONG_TOKEN_CLASS` with `details.accepted`
   (`base_controller.rb:297–305`).
2. **Token shapes are fixed regexes.** `ferry_sk_(sandbox|live)_[0-9A-Za-z]{43}`
   and `ferry_pat_[0-9A-Za-z]{43}` (`lib/ferry/tokens.rb:57–58`).
3. **The `Idempotency-Key` is 1–255 bytes of printable ASCII, and the server
   never mints one** (`command_request.rb:116–117`, `207–215`).
4. **Execute's preflight order is: existing key → gateway available → plan.**
   `Executor#preflight` (`executor.rb:193–201`) looks the caller key up first,
   then answers `UPSTREAM_NOT_CONFIGURED` if the gateway cannot send
   (`:230–232`), then checks the plan (`:239–256`).
5. **The quote is re-read from upstream before the plan is consumed, and a
   failure there is `UPSTREAM_BUSY` with no command and no `Ferry-Command-Id`**
   (`executor.rb:272–294`); `#validate_quote` (`:300–315`) returns
   `PLAN_CHANGED` or `UPSTREAM_CONTRACT_VIOLATION`, neither touching the plan.
6. **`Ledger::Begin` consumes the plan before it reads the policy; `Refused`
   rolls back (plan untouched — `PLAN_NOT_FOUND`, `PLAN_ALREADY_USED`,
   `PLAN_EXPIRED`, `PLAN_KEY_MISMATCH`, `PLAN_CHANGED`), `Denied` commits a
   `failed_terminal` row with the plan consumed** (`begin.rb:47–61`, `78–89`,
   `331–338`, `353–402`, `479–485`; `DENIAL_STATUS = 403` at `:103`).
7. **The policy verdicts are exactly the `CHECKS` list** (`evaluate.rb:253–325`).
   `DESTINATION_TOO_NEW` is among them (`:295`) but **fires only when
   `destination_usable_after` is non-nil, and `Ledger::Begin`'s signature
   defaults it to `nil` with no production caller supplying it**
   (`begin.rb:206–207`; comment at `evaluate.rb:292–294`: "the mirror, which
   does not exist yet"). It is unreachable from any HTTP request today.
8. **Simulate never evaluates spend policy.** Its 403s are `EXECUTION_SUSPENDED`
   (`simulator.rb:107`, `277–288`) and the authentication codes; its 422s come
   from `Corridors::QuoteRequest`. Neither transfer endpoint emits
   `STEP_UP_REQUIRED`; only `ApiKeyPolicy` does (`api_key_policy.rb:109–118`).
9. **The simulate 201 body is `{object: "simulation", command_id, quote, plan:
   {object, id, expires_at}}` plus `token` on the fresh answer only**
   (`Simulated#rendered_body`, `simulator.rb:125–129`). **The stored body —
   what a replay and what `GET /v1/commands/{id}.result.body` return — is
   `CompleteQuote#stored_body` (`complete_quote.rb:238–249`): no `token`, and
   no top-level `status`.** The replay adds `meta.remediation`
   (`command_request.rb:132–135`, `325–334`).
10. **The execute 201 body is the projected OMS transaction**, whose allowlist
    includes `id`, `status`, `subStatus`, `error.code`, `error.recoverable`,
    `pricing.*`, `estimatedArrival` (`lib/transfers/projection.rb:55–104`).
11. **The command body has fourteen keys**, including `operation`
    (`operation_id`, e.g. `transfers_execute`), `poll` (a path), `result`
    (populated only for `completed` inside the replay window), `contradiction`,
    `transaction_id`, `idempotency_key` (`app/models/command.rb:190–206`,
    `stored_result` at `:209–213`). Poll intervals `1` for `reserved`/`inflight`,
    `5` for `upstream_unknown`/`failed_retriable` (`ledger/replay.rb:68–78`).
12. **`Ferry-Command-Id` is set on 201, 202, 409 in-progress, 503 `UPSTREAM_BUSY`
    from a never-sent pair, 403 policy denials, and any `Refused` whose
    `details.command_id` is set** (`set_command_id` at `command_request.rb:277`,
    `291`, `306`, `326`, `343`, `381`, `395`, `407`, `422`; `Executor` merges
    `command_id` into replay refusals at `executor.rb:220–224`).
    `Idempotency-Key` is echoed before the result is rendered
    (`command_request.rb:120`, `162`) and survives an error render
    (`base_controller.rb:614–619`).
13. **Unknown body keys are refused on the key endpoints** by
    `BaseController#request_body` (`base_controller.rb:530–543`) **and on
    simulate** by the translator (`quote_request.rb:247`, `256`). **Execute
    does not refuse them**: `TransfersController#execute` passes
    `request.request_parameters` straight to the digest and reads `plan_token`
    from it (`transfers_controller.rb:66–73`), and `ExecuteRequest` has no
    `additionalProperties: false` (`openapi.yaml:721–736`). An extra key on
    execute is digested — so it changes the idempotency digest — and otherwise
    ignored. *(Corrected in revision 2; CRITIQUE N4.)*
14. **The key-creation body is `{name, environment, scopes, expires_at}`**
    (`api_keys_controller.rb:29`); revoke takes `{reason}`; list takes
    `environment` and `status ∈ {active, all}`. Pages default to 20 and clamp
    to 1..100 (`pagination.rb:36–38`).
15. **There is no endpoint that mints a personal access token.** PATs come from
    `bin/rails "ferry:pat:issue[…]"` (`lib/tasks/ferry_pat.rake:1–11`), which
    refuses to run except from a command line (`:27–40`) and prints the token
    on its own stdout line (`:158–170`). `ferry auth login` stores a token an
    operator already minted.
16. **There is no rake task that provisions an organization.**
    `Organizations::Provision` creates the two environments and NULL-cap,
    unsuspended policy rows when no restore is open (`provision.rb:75–80`,
    `94–96`); nothing under `lib/tasks/` or `bin/` calls it.
17. **The fixture backend serves a sandbox environment that is not `active`**
    (`fixture.rb:181–183`, `REFUSED_STATUS` at `:127`); a fresh environment is
    `unconfigured` (`db/structure.sql:17–25`) and authenticates
    (`resolver.rb:189–192`). `FERRY_OMS_BACKEND=fixture` selects it outside
    production (`backend.rb:38–45`).
18. **A sandbox policy row with NULL caps executes** (`environment_policy.rb:86–90`;
    `spend_policy/key_policy.rb:63–72`; `amount_basis.rb:62`).
19. **Corridor ids are composite** — `walletCrypto->bankUs` (`corridors.rb:172`,
    `202`); the `>` travels as `%3E`.
20. **Rate-limit buckets** (`buckets.rb:52–65`); a `429` carries
    `retry_after_seconds` and `details.bucket`.
21. **`UPSTREAM_CREDENTIALS_INVALID` and `UPSTREAM_AUTH_UNAVAILABLE` have no
    emitter** (`errors.rb:207–216`); since `6de569a`, `errors.md:101–111` says
    so under "Codes you will not see", alongside `DESTINATION_TOO_NEW`.
22. **CI carries `timeout-minutes: 25` on `test` and `15` on `migrations`**
    (`.github/workflows/ci.yml:16`, `137`).
23. **`CLAUDE.md:9` says the Go CLI is "generated from the same OpenAPI
    contract".** The six response bodies at `openapi.yaml:69`, `116`, `134`,
    `159`, `237`, `319` are `schema: { type: object }`. §5.12 and U8.
24. **Go 1.24 is installed locally; goreleaser and golangci-lint are not.**
25. **`IDEMPOTENCY_KEY_REUSED` has two reasons.** `Replay#refuse`
    (`replay.rb:103–118`) returns it for `different_request` (digest mismatch,
    `:127–132`) and for `different_credential` — the presenting key is neither
    the command's key nor linked to it through `KeyChain` (`:138–142`).
    `POST /v1/api_keys` has no "replaces" argument (`api_keys_controller.rb:29`),
    so a key minted with `ferry keys create` is never linked. The response
    carries `details.reason` and, via `Executor`, `details.command_id`
    (`executor.rb:220–224`). The command under the key may be `completed`.
26. **`IDEMPOTENCY_KEY_REPLAY_EXPIRED` carries `details.expired_at` and
    `details.state`** (`replay.rb:114–117`); `replay_expires_at` is set at
    `Begin` (`begin.rb:147`), so the expired command may be in any state.
    `spec/requests/api/v1/command_request_spec.rb:339` has
    `expire_replay_window!`.
27. **`Executor#send_and_record` (`executor.rb:342–361`) writes a `reserved`
    row and sends `create_transaction` before the caller sees a byte of the
    answer.** Any client failure after the request was written — including a
    signal delivered to the client — leaves a server-side command that may
    execute.
28. **The renderers for the four untyped bodies exist and are single-sourced**:
    `Api::V1::PrincipalSerializer`, `Api::V1::ApiKeySerializer`
    (`app/serializers/api/v1/`), `Transfers::Projection::ALLOWLIST`
    (`lib/transfers/projection.rb:55`), `Quotes::Projection::ALLOWLIST` plus
    `CompleteQuote#stored_body` (item 9). `spec/docs/api_docs_spec.rb` already
    pins `TransferRequest` (`:258–264`), `CreateApiKeyRequest` (`:369–392`)
    and `Command.state` (`:465`) the same way.
29. **`syscall.Flock` is in the Go standard library on darwin and linux**
    (`go doc syscall.Flock`). `golang.org/x/sys` is not needed for the lock.
30. **`flock(2)` locks an inode; `rename(2)` replaces the directory entry.** A
    lock on `<run>.json` does not survive the file's first rewrite by
    temp+rename. (CRITIQUE M7; standard Unix semantics, not re-measured.)

### 1.3 What the CLI can do against today's deployment

| Command | Today | Why |
| --- | --- | --- |
| `ferry auth login` with a PAT an operator minted | Works | `GET /v1/me` accepts a PAT (item 1). |
| `ferry keys list / get / create --env sandbox / revoke` | Works, any of the six scopes | Step-up guards only `live` + `{money:execute, destinations:manage}` (item 8). |
| `ferry keys create --env live --scopes money:execute` | `403 STEP_UP_REQUIRED`, exit 3 | Impossible by construction. |
| `ferry corridors list / get` | Works | 19 rows, 18 quotable. |
| `ferry transfers create` (simulate) | `503 UPSTREAM_NOT_CONFIGURED`, exit 3 | No Polygon credential is attached, sandbox included. |
| `ferry transfers execute --plan …` | `503 UPSTREAM_NOT_CONFIGURED`, exit 3 | Item 4: refused before the plan is looked up. |
| `ferry commands get / watch` | Works for a command that exists | None can exist until the above changes. |

The money path is fully exercisable against a FERRY booted with
`FERRY_OMS_BACKEND=fixture` (items 17–18). That is what the e2e job does and
what `cli/README.md` documents. The CLI does not special-case it.

### 1.4 Documentation defects found while planning — status at `3239f06`

Commit `6de569a` fixed five of the six defects revision 1 reported. Recorded
here so U7's check (AC67, AC89) tests what is *still* true rather than filing a
stale list.

| # | Defect (revision 1) | Status | Evidence |
| --- | --- | --- | --- |
| 1 | `DESTINATION_TOO_NEW` marked `retriable: true` though it spends the plan | **Fixed.** Now `retriable: false` with "simulate again — the plan … is spent" | `errors.rb:263–267`, `errors.md:66` |
| 1b | *(new, CRITIQUE M6)* `DESTINATION_TOO_NEW` is unreachable from HTTP | **Documented.** "Codes you will not see" names it | `errors.md:111`; item 7 |
| 2 | Contract offered `STEP_UP_REQUIRED` on both transfer 403s and `POLICY_*` on simulate | **Fixed** on both operations | `openapi.yaml:250`, `:337–343` |
| 3 | Six response bodies typed `{ type: object }` | **Fixed.** U8 typed the six (plus `List.data.items`, V8); A393 typed the four object nodes U8 left bare — `Error.error.details`, `Command.result.body` (A321), `Command.last_error`, `Command.contradiction` — and pinned each in both directions | `openapi.yaml` `ErrorDetails`, `StoredSimulation`, `Command.result.body`; `api_docs_spec.rb` "the object nodes the contract had left bare"; `schema_pin_test.go` `TestCommandResultBodyIsTheUnionTheContractDeclares` |
| 4 | Two catalogued codes with no emitter | **Documented** under "Codes you will not see" | `errors.md:101–107` |
| 5 | No PAT bootstrap path in `docs/api/` | **Fixed** | `AGENTS.md:79–83` |
| 6 | `limit` default undocumented | **Fixed** | `openapi.yaml:489` |
| 7 | *(new, CRITIQUE N4)* "Unknown keys are refused" is simulate-only; execute digests and ignores them | **Open**, minor. `AGENTS.md` states the rule generally | item 13 |
| 8 | `AGENTS.md:376` and `README.md:87` state that the CLI does not exist | Becomes false when this slice ships | AC67 |

The shared `Forbidden` response component still says "or a `POLICY_*`
refusal … `STEP_UP_REQUIRED`" (`openapi.yaml:535`); it is a component reused
by non-transfer operations and is not wrong there.

---

## 2. Invariants

Numbered `C1`–`C19`. Every acceptance criterion in §3 names the invariant it
defends. C16–C19 are new in revision 2.

**Money invariants — properties of the request path.**

- **C1 — The idempotency key is durable before the first byte leaves.** For
  every money request the CLI sends, a run record naming the key, the operation
  and the exact body bytes has been written, fsynced and atomically renamed
  into place before `http.Client.Do` is called. *(AC44 kills the process to
  prove it.)*
- **C2 — One intent, one key.** A resumed run sends the stored bytes under the
  stored key. The CLI never mints a second key for a request it has already
  recorded; the only way a different key is sent is a new run the caller
  started. A caller-supplied `--idempotency-key` that the ledger holds against
  a different body is refused before anything is sent.
- **C3 — Nothing the CLI does turns an unknown outcome into a definite one.** A
  local deadline, a transport error after the request was written, an
  unparseable 2xx body, an unparseable or unlisted-status 5xx, a poll that ran
  out of time — every one of these is class `pending`/exit 6 with the run id
  and command id printed.
- **C4 — One table decides the outcome.** Exit code, outcome class, "money
  moved?" and "same key safe?" are computed by `internal/outcome` from
  (operation class, HTTP status, `error.code`, `error.details.reason`,
  headers, command `operation`, command `state`, `contradiction`, transaction
  `status`) and by nothing else. The table covers every code in
  `docs/api/errors.md` for every operation class, both directions.
- **C5 — The CLI composes no key the contract does not declare and rewrites no
  byte the caller supplied.** The flag builder emits exactly the keys the user
  set from the declared set; `--body` is forwarded byte-for-byte and the digest
  is taken over those bytes.
- **C6 — The plan token is a secret the CLI holds for at most one run.** It is
  printed to stdout exactly when the API hands it out; it appears in no log,
  no `--debug` trace and no file — except the execute step of a run record, in
  exactly two fields: `plan_token`, written on a `--broadcast` simulate 201 and
  scrubbed when the step reaches `terminal`, `declined` or `unreachable` or on
  load once `plan.expires_at` is ten minutes past by the local clock; and
  `body`, the verbatim `{"plan_token": …}` bytes written by `Begin` after
  consent, **which is not scrubbed** because `body_sha256` must hold for a
  resume to resend the same bytes (V6). A step that reaches `declined` never
  had a `body`, so a declined token leaves the disk entirely; a sent token in
  `body` is spent or expired by the time the step is terminal. `runs show` and
  `runs list` redact both fields on display. The stored copy of any simulate
  response has `plan.token` removed before it is written.
- **C7 — A credential is sent only to the API URL it was verified against and
  only for the credential class it was stored under.** No API URL is compiled
  into the binary; `auth login` requires one explicitly.
- **C8 — `--broadcast` executes exactly the plan it displayed.** A `PLAN_*` or
  policy refusal on the execute step ends the run; the CLI never re-simulates
  and executes on the caller's behalf. A simulate that leaves via `202` makes
  the execute step `unreachable`: no token was ever issued for it.
- **C9 — Success is never bare.** Every 201 from execute renders `status` and
  `subStatus` and the words "not settled"; a 201 whose `status` is `failed` is
  its own class (exit 8); a non-null `contradiction` on any command is
  `escalate`/exit 7 whatever `state` says.
- **C14 — A retry is the same bytes under the same key, after the server's
  interval.** `429`, `503 UPSTREAM_BUSY`, `503 SERVICE_UNAVAILABLE`, `500` on a
  money request are resent identically after `max(retry_after_seconds, 1)`
  inside a budget. `POST /v1/api_keys` is never retried. The fixture's `599`
  is never retried.
- **C16 — Consent is per invocation and is never stored.** An execute request
  (`POST /v1/transfers`) leaves the process only if *this* invocation's
  command line carried consent (`--broadcast` or `transfers execute` on a
  non-TTY; `--yes` anywhere) or *this* invocation's TTY answered `y` to a
  prompt that showed the plan. A stored `plan`, a stored step state and `runs
  resume` never substitute. The prompt is recorded as a step state
  (`awaiting_confirmation`) *before* it is shown, and a decline, EOF, or
  `SIGINT`/`SIGTERM`/`SIGHUP` while it is up writes `declined`. The same rule
  binds all three arms that send an execute: `--broadcast`, `transfers
  execute --plan`, and `runs resume`.
- **C17 — Once a money request may have left, no abnormal exit path says
  otherwise.** A process-wide flag is set immediately before the first
  `api.Do` on a money step **and is never cleared for the life of the
  process** (V1). It governs only *abnormal* exits — signal handler, panic
  recovery, `Record` error, renderer error, any error that reaches the root
  after the flag is set — and every one of those consults it: set → exit 6
  with the run id, the command id if known, and "a money request was sent in
  this invocation; `ferry runs show <id>`"; never exit 1. The normal path
  exits on its outcome class regardless of the flag.
- **C18 — A money command fails closed on an orphaned unresolved run.** Before
  minting a run, a money command looks for runs against the same profile and
  environment whose execute or simulate step is `awaiting_confirmation`,
  `pending`, or `answered` with class 5/6, and whose sidecar lock is *not*
  held by a live process. If any exists it exits 1 naming them and `ferry runs
  resume`, unless `--allow-pending` was passed.
- **C19 — A run is resumed only by the credential that started it.** `runs
  resume` compares the profile's `token_prefix` and `principal_id` to the
  record's before any request; a mismatch is exit 7 naming the prefix to log
  back in with. `IDEMPOTENCY_KEY_REUSED` with `details.reason ==
  "different_credential"` on a money operation is `escalate`/7, never
  "retry with a new key".

**Local-state invariants.**

- **C10 — JSON mode writes exactly one JSON document to stdout on every exit
  path** — success, API refusal, usage error, CLI fault, signal, panic — and
  nothing else reaches stdout. `outcome.exit_code` equals the process exit
  code.
- **C11 — `null` and `[]` survive.** `allowed_assets`/`allowed_networks`
  decode to a nullable slice and render as `null` or `[]` respectively.
- **C12 — Local state is written atomically and read defensively.**
  Directories are 0700 and files 0600; a credentials file wider than 0600 is
  refused; every write is temp-file + fsync + rename; a run is guarded by a
  sidecar `<run_id>.lock` that is never renamed or replaced while its record
  exists and is deleted only by `prune`, under its own lock, after the record
  (V5). A holder takes `LOCK_EX|LOCK_NB` from `Open` through the last
  `Record`; a probe (`Orphans`, `Scrub`) acquires and releases within the
  call; a resume retries the acquire briefly before reporting `ErrRunLocked`,
  so a probe's millisecond cannot masquerade as a live holder. The loser
  stops rather than racing.
- **C15 — The credential class an endpoint needs is decided from a table
  before the request**, and a profile lacking that class is refused locally
  with the remediation named.

**Verification invariants.**

- **C13 — No Go test asserts against a hand-written FERRY response *body*.**
  Every body under `cli/testdata/recorded/` was produced by
  `spec/cli/recorded_interactions_spec.rb` driving the real Rack app and
  asserted against a declared expectation before it was written; the Go
  fixture refuses an unrecorded request loudly. Transport faults (hangs,
  truncation, connection refused, synthetic status lines with no body) are
  synthesised by `httptest` and are not FERRY responses.

---

## 3. Acceptance criteria

Every criterion names the test file that verifies it, the unit that owns it,
and the invariant it defends. **Each has a named mutation in §6.2 that must
make it fail, or is declared a non-control in §6.3.** AC numbers are stable
across revisions; criteria amended in revision 2 are marked *(v2)*, new ones
are AC69 onward. Go test files are under `cli/`; Ruby specs under `spec/`.

### 3.1 Recorded interactions (U0)

- **AC1 — Every scenario in §5.10 is recorded** and the manifest lists exactly
  that set, both directions. *Spec:* `spec/cli/recorded_interactions_spec.rb`.
  *Defends:* C13.
- **AC2 — Recordings are byte-stable across two runs.** Volatile values are
  replaced by typed placeholders through an allowlisted substitution, and the
  spec records each scenario twice in one example and asserts equality.
  *Spec:* same file. *Defends:* C13.
- **AC3 — A recording carries the request's method, path, `{Idempotency-Key
  present?, credential class}`, and the placeholder-substituted body; and the
  response's status, the header subset `{Ferry-Command-Id, Idempotency-Replayed,
  Idempotency-Key, Retry-After, Ferry-Environment, WWW-Authenticate}` and the
  body.** *(v2: `body_sha256` dropped; §5.10.)* *Spec:* same file. *Defends:*
  C13.
- **AC4 — Recording writes nowhere but `cli/testdata/recorded/`** and CI fails
  if the committed files differ from what the suite produced (`git diff
  --exit-code` in the `test` job). *Spec:* same file; CI step in U6.
  *Defends:* C13.
- **AC76 — Each scenario declares an expectation and the recorder asserts it
  before writing** *(v2, CRITIQUE M5)*: `scenarios.rb` gives every scenario
  `expect: [{status:, code:, headers_present: [], headers_absent: [], body_has:
  [], body_lacks: [], body_equals: {}}]` — an **array**, one entry per
  interaction, because the four sequence scenarios answer more than once and a
  single object would collapse to whatever the last answer was (A301); and
  `body_equals` is a path-to-value map, because the value assertion this AC
  itself demands below cannot be written with presence axes alone (A300). The
  recorder fails if the Rack app's answer disagrees; the manifest carries the
  expectation. At minimum: `execute.policy_denied.403`
  expects `code =~ /^POLICY_/` and `Ferry-Command-Id` present;
  `execute.upstream_busy.with_command.503` expects `Ferry-Command-Id` present;
  `execute.upstream_busy.no_command.503` expects it absent;
  `simulate.replay.201` expects `body_lacks: [plan.token]` and `body_has:
  [meta.remediation]`; `simulate.201` expects `body_has: [plan.token]`;
  `execute.key_reused.different_credential.409` expects `details.reason ==
  "different_credential"`. *Spec:* same file. *Defends:* C13.

### 3.2 Local state: paths, credentials, run ledger, ownership (U1)

- **AC5 — Run ids are 26-character Crockford ULIDs from `crypto/rand`,
  monotonic within a process.** Two independent generators over 10,000 draws
  produce no collision. *Test:* `internal/ulid/ulid_test.go`. *Defends:* C2.
- **AC6 — Paths resolve `FERRY_HOME` > `XDG_CONFIG_HOME`/`XDG_STATE_HOME` >
  `~/.config/ferry`, `~/.local/state/ferry`; directories are created 0700.**
  *Test:* `internal/xdg/paths_test.go`. *Defends:* C12.
- **AC7 — The credentials file is written 0600 by temp+fsync+rename, and a file
  wider than 0600 is refused on read** with `ErrPermissionsTooWide`. *Test:*
  `internal/creds/store_test.go`. *Defends:* C12.
- **AC8 — A profile holds one credential per class; `Put` classifies by the
  §1.2.2 regexes with no I/O; `For(Requirement)` returns the slot the
  requirement names or `ErrNoCredentialOfClass`.** *Test:* same file.
  *Defends:* C15.
- **AC9 — A credential is bound to the API URL it was stored with** *(v2)*;
  `For` with a different URL returns `ErrAPIURLMismatch` and not the token; a
  profile with no `api_url` cannot be created. *Test:* same file. *Defends:*
  C7.
- **AC10 — `runs.Begin` writes the record before returning, in the order
  write-temp → fsync → rename → fsync(dir), mode 0600**, proven against a
  fault-injecting filesystem that records the syscall sequence and can fail at
  each step; a failure leaves no record at the final path. *Test:*
  `internal/runs/ledger_test.go`. *Defends:* C1.
- **AC11 — `runs.Open` yields exactly the stored body bytes and key, and
  refuses a record whose stored `body_sha256` disagrees with its stored body**
  (`ErrRecordCorrupt`). *Test:* same file. *Defends:* C2.
- **AC12 — The run lock is the sidecar `<run_id>.lock`, and a second `Open`
  is refused *after* the first holder has rewritten the record at least once**
  *(v2, CRITIQUE M7)*: process A opens, calls `Record` (rename), and holds;
  process B's `Open` returns `ErrRunLocked` after retrying `LOCK_NB` for at
  most 250 ms *(v2.1, V5)*. Conversely, a probe (`Orphans`) that acquires and
  releases in another process while B is retrying does **not** make B fail.
  *Test:* same file, two processes. *Defends:* C12.
- **AC13 — `Scrub(now)` replaces the execute step's `plan_token` with
  `"[SCRUBBED]"` when the step is `terminal`, `declined` or `unreachable`, or
  when `plan.expires_at + 10m < now`; `body_sha256` is unchanged; a run whose
  lock is held is skipped** *(v2, CRITIQUE N2)*. *Test:* same file.
  *Defends:* C6.
- **AC14 — `FindByKey(key)` returns the run and step that own a caller-supplied
  key by exact match**, or nothing. *Test:* same file. *Defends:* C2.
- **AC82 — `runs.Record` strips `plan.token` from a simulate response before
  writing** *(v2, CRITIQUE G3; v2.1, V6)*: the stored `response.body.plan` has
  no `token` key; the token appears in the record only in the execute step's
  `plan_token` field and, once `Begin` has run for that step, in its verbatim
  `body`; a sweep over a `--broadcast` run record at each state
  (`awaiting_confirmation`, `declined`, `pending`, `terminal`) asserts exactly
  those occurrences and no other. *Test:* same file. *Defends:* C6.
- **AC92 — `runs.Prune` deletes the record before the sidecar, holding the
  sidecar lock through both; `runs.Decline(run)` writes `declined` and scrubs
  `plan_token` for an execute step in `awaiting_confirmation` or
  `not_started`, and refuses (`ErrStepMayHaveSent`) for any step in `pending`
  or later** *(v2.1, V4, V5)*. *Test:* same file. *Defends:* C12, C16.
- **AC84 — Every file under `cli/` matches exactly one ownership prefix in
  `cli/OWNERSHIP`, and every prefix in `cli/OWNERSHIP` names a unit in §4.1**
  *(v2, CRITIQUE M3)*. Runs in every `go test ./...`. *Test:*
  `cli/ownership_test.go`. *Defends:* — (process).
- **AC90 — `runs.Orphans(profile, env)` returns runs whose simulate or execute
  step is `awaiting_confirmation`, `pending` or `answered` with class 5/6 and
  whose sidecar lock can be acquired; a run whose lock another process holds is
  not an orphan; the probe releases each lock before returning; an `exempt`
  run id is never reported** *(v2, CRITIQUE M9; v2.1, V3, V5)*. *Test:* same
  file, two processes. *Defends:* C18.

### 3.3 The API client and the decision table (U2)

- **AC15 — Every request carries `Authorization: Bearer <token>`, `Accept:
  application/json`, `User-Agent: ferry-cli/<version> (<os>/<arch>; go<ver>)
  contract/<sha8>`; every request with a body carries `Content-Type:
  application/json` and the exact bytes it was given.** *Test:*
  `internal/api/client_test.go`. *Defends:* C5.
- **AC16 — `Idempotency-Key` is sent on exactly the operations `openapi.yaml`
  declares the `IdempotencyKey` parameter for**, both directions. *Test:*
  `internal/api/contract_test.go`. *Defends:* C4.
- **AC17 — The error envelope decoder requires all seven keys** and returns
  `ErrEnvelopeShape` otherwise; the key list equals `Error.error.required` in
  `openapi.yaml`. *Test:* same file. *Defends:* C4.
- **AC18 — Response metadata reads the six headers; absent `Retry-After` is
  `nil`; `Ferry-Environment` differing from the credential's environment sets
  `Meta.EnvironmentMismatch`.** *Test:* `internal/api/meta_test.go`.
  *Defends:* C4, C9.
- **AC19 — `outcome.Classify` covers every code in `docs/api/errors.md` for
  each of the four operation classes**, parsed from the generated table, both
  directions. Codes under "Codes you will not see" are mapped and are marked
  `unreachable: true` in the table so the coverage test can also assert that
  no recorded scenario claims to exercise them. *Test:*
  `internal/outcome/coverage_test.go`. *Defends:* C4.
- **AC20 — Every row of §5.6 is one example asserting `(class, exit, money,
  same_key_safe, next)`.** *(v2 additions in bold.)* At least: policy denial
  and `DESTINATION_TOO_NEW` on execute → 4; `EXECUTION_SUSPENDED` on simulate →
  3; `PLAN_*` on execute → 4; `UPSTREAM_UNAVAILABLE` → 4;
  `UPSTREAM_NOT_CONFIGURED` → 3; `UPSTREAM_BUSY` without `Ferry-Command-Id` →
  5, with it → 6; `IDEMPOTENCY_KEY_IN_PROGRESS` → 6; **`IDEMPOTENCY_KEY_REUSED`
  + `different_request` → 3, + `different_credential` → 7, + absent reason →
  7**; `IDEMPOTENCY_KEY_REPLAY_EXPIRED` → 7 with `details.state` in `next`;
  `COMMAND_UNRESOLVED` → 7; `429` → 5; `500` on money → 6, on read → 5;
  **unknown code on read: 4xx → 3, 5xx → 5**. *Test:*
  `internal/outcome/table_test.go`. *Defends:* C4, C19.
- **AC21 — A non-null `contradiction` classifies `escalate`/7 for every command
  state.** *Test:* same file. *Defends:* C9.
- **AC22 — A 201 from execute classifies by the body's `status`: `failed` →
  8; anything else → 0 with `money == "accepted_upstream"`.** A 201 from
  simulate is `done`/0. *Test:* same file. *Defends:* C9.
- **AC23 — Transport errors are split by whether bytes may have left**: dial
  errors are `transient`/5 for money; anything after the request was written
  is `pending`/6. *Test:* `internal/api/transport_test.go`. *Defends:* C3.
- **AC24 — A code the table does not know is `pending`/6 on a money request;
  on a read it is 3 for a 4xx and 5 for a 5xx** *(v2, CRITIQUE B1)*. *Test:*
  `internal/outcome/table_test.go`. *Defends:* C3.
- **AC25 — Contract pins**: operation set equals the `(method, path)` pairs in
  `openapi.yaml` minus `/oms/webhooks/{locator}` and `/v1/{path}`;
  `Command.state` enum equals the Go state set; `TransferRequest`,
  `TransferSide`, `CreateApiKeyRequest` property sets equal the builders'
  declared sets; the `scopes` enum equals `keys.Scopes`. *Test:*
  `internal/api/contract_test.go`. *Defends:* C5.
- **AC26 — Corridor decoding preserves `null` vs `[]`.** *Test:*
  `internal/api/corridor_test.go`. *Defends:* C11.
- **AC27 — Retry policy**: `429`, `503 UPSTREAM_BUSY` (no command), `503
  SERVICE_UNAVAILABLE`, `503 UPSTREAM_AUTH_UNAVAILABLE`, `500` on a money
  request are resent with byte-identical body and identical key after
  `max(retry_after_seconds, 1)` (fake clock) within `--retry-budget` (default
  60 s); `control_create` is never retried; **`599` is never retried** *(v2,
  CRITIQUE N5)*. *Test:* `internal/api/retry_test.go`. *Defends:* C14.
- **AC28 — `--debug` traces redact `Authorization`, every `ferry_sk_`/
  `ferry_pat_`/`ferry_plan_` token, and every `"token"` value**, with canaries.
  *Test:* `internal/api/redact_test.go`. *Defends:* C6.
- **AC71 — `details.reason` is an input to the table** *(v2, CRITIQUE B2)*:
  `409 IDEMPOTENCY_KEY_REUSED` on a money operation classifies
  `refused_fix`/3 only when `reason == "different_request"`; `different_credential`
  and any other or absent reason classify `escalate`/7 with `next` naming
  `details.command_id`. *Test:* `internal/outcome/table_test.go`. *Defends:*
  C19.
- **AC74 — The terminal command rule is selected by the body's `operation`**
  *(v2, CRITIQUE B4)*: `completed` with `operation == transfers_execute` →
  classify `result.body.status` (0 or 8); `completed` with `operation ==
  transfers_simulate` → `refused_resimulate`/4, `next` = "FERRY completed this
  simulation after the request ended; the plan token was never issued —
  simulate again under a new key"; `result == null` → 0 with "no longer
  replayable". *Test:* same file. *Defends:* C8.
- **AC83 — A money response whose body is not the seven-key envelope, or
  whose status has no row (e.g. `413`, `415`, HTML `502`), is `pending`/6; on
  a read it is `transient`/5** *(v2, CRITIQUE G4)*. *Test:* same file.
  *Defends:* C3.
- **AC86 — The Go structs for `Principal`, `ApiKey`, `Simulation`, `Quote`,
  `Transaction` have field sets equal to the corresponding `openapi.yaml`
  schemas' `properties`, both directions, recursively** *(v2; v2.1, V2)*; a
  struct field with no schema property, or a property with no field, is red.
  Conditional properties (`ApiKey.token`, `Simulation.plan.token`,
  `Simulation.meta`) are pointer fields so absence decodes as `nil`. *Test:*
  `internal/api/schema_pin_test.go`. *Defends:* C4. *Depends on U8.*

### 3.4 The fixture server (U3)

- **AC29 — The loader reads every file under `cli/testdata/recorded/`, refuses
  a file the manifest does not name and a manifest entry with no file, and the
  server answers an unrecorded request with status 599 and header
  `X-Fixture-Unrecorded: <method> <path>`.** *Test:*
  `internal/fixture/server_test.go`. *Defends:* C13.
- **AC30 — A money `POST` matches on method, path, presence of
  `Idempotency-Key`, and *structural* JSON equality of the body after
  placeholder substitution** *(v2, CRITIQUE M4)*: key order and whitespace are
  ignored; an extra key, a missing key or a different value is unrecorded.
  *Test:* same file. *Defends:* C5, C13.
- **AC31 — A scenario may be a sequence replayed in order, and the server
  records every request it received** (headers, raw bytes, sha256) so a test
  can assert count, spacing, keys and byte identity between two requests.
  *Test:* same file. *Defends:* C13.
- **AC32 — The server never invents `Ferry-Command-Id`, `Retry-After` or
  `Idempotency-Replayed`.** *Test:* same file. *Defends:* C13.
- **AC77 — The loader asserts each recording against its manifest
  expectation** *(v2, CRITIQUE M5; v2.1, A300/A301)* (`status`, `code`,
  `headers_present/absent`, `body_has/lacks`, **and `body_equals`**) and
  refuses to serve a directory in which any file disagrees. `expect` decodes as
  an **array**, one entry per interaction. An expectation axis the loader does
  not decode is one U0 asserted Ruby-side and the fixture silently drops, so an
  unrecognised key in an entry must be an error rather than ignored. *Test:*
  same file. *Defends:* C13.
- **AC91 — The fixture may hold a request open until released** *(v2, CRITIQUE
  B1)*: `server.Hold(scenario)` returns a release function; a test can deliver
  a signal to the client while its request is on the wire. *Test:* same file.
  *Defends:* C17.

### 3.5 Auth, keys, corridors, rendering, pre-check (U4)

- **AC33 — `auth login` reads the token from `--token-stdin` or a TTY prompt
  with echo off; there is no `--token <value>` flag.** *Test:*
  `internal/noun/auth/login_test.go`. *Defends:* C6.
- **AC34 — `auth login --env <kind>` refuses an API key of another environment
  (exit 3, zero requests) and refuses `--env` with a PAT (exit 2).** *Test:*
  same file. *Defends:* C7.
- **AC35 — `auth login` requires `--api URL` or `FERRY_API_URL`** *(v2)* —
  neither set is exit 2 with "no API endpoint configured; pass `--api` or set
  `FERRY_API_URL`" and zero requests — **calls `GET /v1/me` there and stores
  only on 200**; the stored record carries `api_url`, `token_prefix`,
  `environment`, `principal_id`. *Test:* same file. *Defends:* C7.
- **AC36 — `auth whoami` renders both slots with prefix and last4 only; `auth
  logout [--class]` removes the named slot or both.** *Test:*
  `internal/noun/auth/whoami_test.go`. *Defends:* C6.
- **AC37 — `keys create` sends exactly the keys the caller supplied**, prints
  the token once, and with `--login` stores it after the 201. *Test:*
  `internal/noun/keys/create_test.go`. *Defends:* C5.
- **AC38 — `keys create` is never auto-retried**; a transport error after
  write is exit 6 with "run `ferry keys list` before minting again". *Test:*
  same file. *Defends:* C14.
- **AC39 — `keys list` passes `environment`, `status`, `limit`, `cursor`
  through unchanged and follows `next_cursor` only under `--all`.** *Test:*
  `internal/noun/keys/list_test.go`. *Defends:* C5.
- **AC40 — `corridors get walletCrypto->bankUs` requests
  `/v1/corridors/walletCrypto-%3EbankUs`**, and the list render shows `null`
  and `[]` distinct. *Test:* `internal/noun/corridors/get_test.go`.
  *Defends:* C11.
- **AC41 — *(v2: moved to U5, CRITIQUE M3)* In JSON mode exactly one JSON
  document reaches stdout for: a 200, an API error, a usage error, a CLI
  fault, a refused pre-check, a panic in a renderer, and SIGINT; stdout
  contains nothing else; `outcome.exit_code` equals the observed exit code.**
  *Test:* `internal/cli/json_test.go`. *Owner:* U5. *Defends:* C10.
- **AC42 — Text mode prints a secret in exactly two places** — the `keys
  create` token and the simulate `plan.token`. *Test:*
  `internal/render/secrets_test.go`. *Defends:* C6.
- **AC43 — A command whose endpoint requires a class the profile lacks exits 3
  with zero requests and names the remediation.** *Test:*
  `internal/noun/precheck_test.go`. *Defends:* C15.

### 3.6 The money path (U5)

- **AC44 — The run record is on disk before the request is sent.** With
  `FERRY_CLI_FAULT=after_record_written` the process exits before `Do`; the
  record is `pending`; the fixture received zero requests. *Test:*
  `internal/noun/transfers/crash_test.go`. *Defends:* C1.
- **AC45 — `runs resume --yes` after `FERRY_CLI_FAULT=after_send_before_record`
  sends byte-identical body under the identical key** (fixture saw the same
  key and sha256 twice), classifies, and moves the record to terminal. *Test:*
  same file. *Defends:* C2.
- **AC46 — `transfers create` without `--broadcast` sends only simulate,
  prints `plan.token` once to stdout, and stores no token anywhere in the run
  record** (execute step absent; stored response stripped). *Test:*
  `internal/noun/transfers/create_test.go`. *Defends:* C6.
- **AC47 — `--broadcast` pre-mints both step keys before simulate is sent,
  executes exactly the `plan.token` from the fresh 201, and on a `PLAN_*`,
  policy or `EXECUTION_SUSPENDED` refusal exits 4 having sent exactly one
  simulate.** *Test:* `internal/noun/transfers/broadcast_test.go`. *Defends:*
  C8.
- **AC48 — On a TTY without `--yes`, `--broadcast` writes
  `awaiting_confirmation`, shows the quote, corridor, environment and
  `plan.expires_at`, and asks; `n` writes `declined`, scrubs the token and
  exits 1 having sent no execute. On a non-TTY stdin no prompt text is
  written and the execute is sent** *(v2, CRITIQUE N6)*. *Test:* same file
  (pty for the TTY half). *Defends:* C16.
- **AC49 — Every execute 201 render contains `status`, `subStatus`, "not
  settled", and neither "success" nor "✓".** *Test:*
  `internal/noun/transfers/render_test.go`. *Defends:* C9.
- **AC50 — A 202 is followed by polling every `retry_after_seconds` (fake
  clock; `[5, 5, 1]` for the recorded sequence) until terminal; the terminal
  rule is AC74's, selected by the body's `operation`; `--timeout` elapsing
  exits 6 with the command id** *(v2)*. *Test:*
  `internal/noun/transfers/poll_test.go`. *Defends:* C3, C8.
- **AC51 — `needs_operator`, `COMMAND_UNRESOLVED` and any non-null
  `contradiction` stop polling and exit 7.** *Test:* same file. *Defends:* C9.
- **AC52 — `UPSTREAM_BUSY` with `Ferry-Command-Id` resends the same key after
  `retry_after_seconds`, receives the recorded 202, and polls; without the
  header it retries within the budget and exits 5.** *Test:*
  `internal/noun/transfers/busy_test.go`. *Defends:* C14.
- **AC53 — `IDEMPOTENCY_KEY_IN_PROGRESS` polls `details.command_id` and never
  resends under a new key**: the fixture saw exactly one `POST` and its
  `Idempotency-Key` equals the run's. *Test:* same file. *Defends:* C2.
- **AC54 — `--idempotency-key K` where the ledger holds K against a different
  body exits 3 with zero requests; with the same body it resumes that run.**
  *Test:* `internal/noun/transfers/userkey_test.go`. *Defends:* C2.
- **AC55 — `execute --plan <token|->` sends exactly `{"plan_token": "…"}`.**
  *Test:* `internal/noun/transfers/execute_test.go`. *Defends:* C5.
- **AC56 — The flag builder emits exactly the keys the caller set; `--body`
  bytes are forwarded unchanged (non-canonical whitespace survives to the
  wire); `--body` with any field flag is a usage error.** *Test:*
  `internal/noun/transfers/body_test.go`. *Defends:* C5.
- **AC57 — `runs list` and `runs show` never print a plan token — `plan_token`
  and `body.plan_token` both render as `[REDACTED]` at every step state; `runs
  prune --older-than` removes only terminal, scrubbed records (`declined` and
  `unreachable` included, `awaiting_confirmation` excluded); `runs decline
  <id>` writes `declined` for an execute step that has not run `Begin` and
  refuses one that has, naming `runs resume`** *(v2.1, V4, V6)*. *Test:*
  `internal/noun/runs/runs_test.go`. *Defends:* C6, C16.
- **AC58 — A money command with `--env <kind>` refuses a credential of another
  kind before sending; `LIVE` prefixes every outcome line for a live
  credential.** *Test:* `internal/noun/transfers/env_test.go`. *Defends:* C7.
- **AC59 — There is one poll loop**: an AST sweep finds no request to
  `/v1/commands/` and no `time.Sleep` outside `internal/poll`. *Test:*
  `internal/poll/sweep_test.go`. *Defends:* C3.
- **AC69 — An abnormal exit after a money request may have left is exit 6,
  never 1** *(v2, CRITIQUE B1; v2.1, V1)*: (a) SIGINT delivered while the
  fixture holds the execute request open; (b) `FERRY_CLI_FAULT=record_fails_after_send`
  (the fs seam returns `ENOSPC` from `Record`); (c)
  `FERRY_CLI_FAULT=render_panics_after_201` — the panic fires *after* `Record`
  succeeded; (d) SIGINT delivered during `poll.Watch` after a recorded 202.
  Each exits 6; the step is `pending` for (a) and (b), `answered` with the
  response recorded for (c) and (d); the run id is printed; in JSON mode one
  document is emitted with `outcome.class == "pending"` and `next` naming
  `ferry runs show <id>`. The same fault points in a process that has not yet
  called `Do` on a money step exit 1 (control for the flag's set-point).
  *Test:* `internal/noun/transfers/postsend_test.go`. *Defends:* C17.
- **AC70 — `runs resume` under a different credential exits 7 with zero
  requests** *(v2, CRITIQUE B2)*: the profile's `token_prefix` or
  `principal_id` differs from the record's; the message names the prefix that
  started the run. *Test:* `internal/noun/runs/resume_test.go`. *Defends:*
  C19.
- **AC72 — A kill at the prompt is not consent** *(v2, CRITIQUE B3; v2.1, V4,
  V9)*: `at_prompt` fires **after the prompt text has been written and before
  the read begins**. With `FERRY_CLI_FAULT=at_prompt` the process dies; the
  test asserts the execute step is `awaiting_confirmation` (not `not_started`)
  with the token stored. `runs resume` with stdin closed and no `--yes` exits
  1, sends zero execute requests, leaves the step `awaiting_confirmation`, and
  names both `runs resume --yes` and `runs decline`; `runs resume` on a pty
  re-shows the plan and asks; `n` writes `declined` and scrubs. `SIGHUP`
  delivered to the pty while the prompt is up writes `declined`. *Test:* same
  file (pty). *Defends:* C16.
- **AC93 — `transfers execute --plan` obeys C16 on its own** *(v2.1, V10)*: on
  a pty without `--yes` it writes `awaiting_confirmation` before writing the
  prompt, shows the token prefix and environment, and on `n` writes `declined`
  having sent zero requests; with `--yes` on a pty, and on a non-TTY without
  `--yes`, it sends without prompting; the `at_prompt` fault leaves
  `awaiting_confirmation`. *Test:* `internal/noun/transfers/execute_test.go`
  (pty). *Defends:* C16.
- **AC94 — Same-body `--idempotency-key` resume is not blocked by the orphan
  check** *(v2.1, V3)*: with an orphaned run R holding key K, `transfers
  execute --plan … --idempotency-key K` with the same body routes into
  `resume R` (consent per C16) and is not refused by C18; with a *different*
  orphan S also present and no `--allow-pending`, it is refused naming S.
  *Test:* `internal/noun/transfers/userkey_test.go`. *Defends:* C2, C18.
- **AC73 — `runs resume` never sends an execute step without consent in the
  resuming invocation** *(v2, CRITIQUE B3)*: for an execute step in
  `awaiting_confirmation` or `pending`, non-TTY without `--yes` exits 1 or 6
  respectively having sent nothing; `--yes` sends; a TTY asks. A simulate step
  is resumed without asking. *Test:* same file. *Defends:* C16.
- **AC75 — Under `--broadcast`, a simulate that answers `202` marks the execute
  step `unreachable` at the moment the 202 is recorded; no `plan` or token is
  ever written to it; after polling to `completed` the run exits 4 with AC74's
  simulate sentence and the fixture saw zero execute requests** *(v2, CRITIQUE
  B4)*. *Test:* `internal/noun/transfers/broadcast_test.go`. *Defends:* C8.
- **AC78 — The flag-built body for the canonical simulate request is
  structurally equal to `simulate.201`'s recorded request body** *(v2,
  CRITIQUE M4)*, and the same flags produce byte-identical output on two
  invocations (deterministic serialisation). *Test:*
  `internal/noun/transfers/body_test.go`. *Defends:* C5.
- **AC80 — The two `--broadcast` inter-step windows have fault points** *(v2,
  CRITIQUE M8)*: `after_simulate_recorded_before_execute_begin` → `runs
  resume --yes` sends exactly one execute with the stored token under
  `<run>-execute`, and the fixture saw exactly one simulate;
  `after_simulate_send_before_record` → resume resends simulate, receives the
  recorded replay with no token, marks execute `unreachable`, exits 4, zero
  execute requests. *Test:* `internal/noun/transfers/crash_test.go`.
  *Defends:* C1, C2, C8.
- **AC81 — A money command refuses when an orphaned unresolved run exists for
  the profile and environment** *(v2, CRITIQUE M9)*: exit 1, zero requests, the
  message names the run ids and `ferry runs resume`; `--allow-pending`
  proceeds; a run whose lock a live process holds does not trigger it. *Test:*
  `internal/noun/transfers/pending_test.go` (two processes). *Defends:* C18.
- **AC87 — `--idempotency-key` with `--broadcast` is a usage error (exit 2,
  zero requests)** *(v2, CRITIQUE G2)*. *Test:*
  `internal/noun/transfers/userkey_test.go`. *Defends:* C2.

### 3.7 CI, e2e, release (U6)

- **AC60 — `ci.yml` gains a `cli` job (`timeout-minutes: 10`) and a `cli-e2e`
  job (`timeout-minutes: 20`); the `test` job gains one step, `git diff
  --exit-code cli/testdata/recorded`, and is otherwise unchanged.** *Spec:*
  `spec/ci/workflow_spec.rb`. *Defends:* C13.
- **AC61 — e2e happy path against the real Rails app with
  `FERRY_OMS_BACKEND=fixture`**: bootstrap → `auth login --api` (PAT) →
  `keys create --env sandbox --scopes read,money:simulate,money:execute
  --login` → `corridors list` (19 rows, 18 quotable) → `transfers create`
  (201, a `ferry_plan_` token on stdout) → `transfers create --broadcast
  --yes` (execute 201; `status` rendered) → `runs show` (token scrubbed).
  *Test:* `cli/e2e/happy_test.go`. *Defends:* C1, C6, C9.
- **AC62 — e2e crash-resume at both fault points** *(v2, CRITIQUE M2; v2.1,
  V7)*: at `after_record_written`, `runs resume --yes --output json` produces
  a document with `http.replayed == false`, `http.status == 201`, and a
  terminal run; at `after_send_before_record`, the resume document has
  `http.replayed == true` and a non-null `http.command_id`, and `bin/rails
  runner` reports exactly one `commands` row with `caller_idempotency_key ==
  <run>-execute` in the e2e environment. *Test:* `cli/e2e/resume_test.go`.
  *Defends:* C1, C2.
- **AC63 — e2e refusals** *(v2.1, V7)*: PAT-only profile → refused locally,
  JSON document with `http == null` and `outcome.class == "refused_fix"`; with
  hidden `--no-precheck` → `http.status == 403`, `error.code ==
  "WRONG_TOKEN_CLASS"`, exit 3; `keys create --env live --scopes
  money:execute` → `403 STEP_UP_REQUIRED` → 3; `transfers create --body` with
  an unknown key → `400 VALIDATION_FAILED` → 3. *Test:*
  `cli/e2e/refusals_test.go`. *Defends:* C4, C15.
- **AC64 — `goreleaser build --snapshot --clean` produces the four targets
  with `CGO_ENABLED=0`; `ferry version` prints version, commit, Go version and
  the `openapi.yaml` sha256.** *Test:* `cli/e2e/release_test.go`. *Defends:* —.
- **AC65 — `cli-release.yml` triggers only on tags matching `cli/v*`, carries
  `timeout-minutes`, and publishes `Formula/ferry.rb` to `kurenn/homebrew-tap`.**
  *Spec:* `spec/ci/workflow_spec.rb`. *Defends:* —.

### 3.8 Contract schemas (U8, Ruby) *(v2)*

- **AC85 — `openapi.yaml` gains `Principal`, `ApiKey`, `Simulation`, `Quote`
  and `Transaction` schemas; the **seven** untyped bodies reference them
  (`GET /v1/me` 200 → `Principal`; `POST /v1/api_keys` 201, `GET
  /v1/api_keys/{id}` 200, `POST …/revoke` 200 → `ApiKey`; `List.data.items` →
  `ApiKey`; simulate 201 → `Simulation`; execute 201 → `Transaction`); and
  `spec/docs/api_docs_spec.rb` pins each schema's property set (recursively)
  equal, both directions, to the body producer** *(v2; v2.1, V2, V8)*:
  - `Principal` ↔ the **union** of `PrincipalSerializer.render` over one
    API-key principal and one PAT principal (an API key renders `user: null`,
    a PAT renders `environment: null` — `principal_serializer.rb:44–52`,
    `66–70`), nested shapes taken from whichever render has them non-null.
  - `ApiKey` ↔ `ApiKeySerializer.render(key, token: "x")` keys; `token` is
    documented **present only on the `POST /v1/api_keys` 201**
    (`api_key_serializer.rb:40`), and the spec asserts `render(key)` lacks it.
  - `Simulation` ↔ `CompleteQuote#stored_body` keys ∪ `{plan.token,
    meta.remediation}`; `plan.token` documented **fresh 201 only**,
    `meta.remediation` documented **replayed 201 only**
    (`command_request.rb:329–331`); the spec asserts the fresh render has
    `plan.token` and no `meta`, and the replay has `meta.remediation` and no
    `plan.token`.
  - `Quote` ↔ `Quotes::Projection::ALLOWLIST`; `Transaction` ↔
    `Transfers::Projection::ALLOWLIST`, both as nested property trees.
  U8 touches no serializer: every pin reads rendered objects, never a
  constant the serializers do not have. *Spec:* `spec/docs/api_docs_spec.rb`.
  *Defends:* C4.

### 3.9 Adversarial matrix, audit, documentation (U7)

- **AC66 — Every row of §7.1 is a named test, and the set of test names equals
  the row set, both directions.** *Test:* `cli/internal/adversarial_test.go`.
  *Defends:* all.
- **AC67 — A spec asserts that `docs/api/AGENTS.md` and `README.md` do not
  state that the CLI does not exist, and that `AGENTS.md` links `cli/README.md`**
  *(v2, CRITIQUE G1: a check, not a static edit)*. *Spec:*
  `spec/docs/cli_docs_spec.rb`. *Defends:* —.
- **AC68 — The mutation audit is recorded** in `docs/loops/cli/MUTATIONS.md`
  and §11.2 with an observation per §6.2 row. *Defends:* all. *Non-control
  (§6.3).*
- **AC89 — `DOC-DEFECTS.md` lists only defects still present, and a spec
  re-measures each** *(v2, CRITIQUE G1)*: item 3 (until U8 lands), item 7.
  *Spec:* `spec/docs/cli_docs_spec.rb`. *Defends:* —.

---

## 4. Units, ownership and waves

Every file below belongs to exactly one unit; `cli/OWNERSHIP` (U1) is the
machine-readable form and AC84 enforces it in every wave. A file not listed is
not to be edited by this slice; a unit that finds it must is an amendment
(§11.1) before it is a commit. `cli/go.mod` and `cli/go.sum` are U1's and **no
other unit adds a dependency** (§5.13).

### 4.1 Units

**U0 — Recorded interactions (Ruby).**
Owns `spec/cli/recorded_interactions_spec.rb`, `spec/support/cli/recorder.rb`,
`spec/support/cli/scenarios.rb`, `cli/testdata/recorded/**` (generated),
`cli/testdata/recorded/MANIFEST.json`. Reads, does not edit,
`spec/support/oms/scripted_gateway.rb`, `spec/support/api/command_probes.rb`,
`spec/support/ledger/builders.rb`, `spec/support/credentials/credential_builders.rb`.
`expire_replay_window!` is private to `spec/requests/api/v1/command_request_spec.rb:339`,
not a support file; U0 copies its one `UPDATE commands SET replay_expires_at
…` into `spec/support/cli/recorder.rb` rather than editing either (V11).
Delivers AC1–AC4, AC76. `FERRY_TEST_DATABASE=ferry_test_cli_u0`.

**U1 — Go module, local state, harness, ownership lint.**
Owns `cli/go.mod`, `cli/go.sum`, `cli/OWNERSHIP`, `cli/ownership_test.go`,
`cli/internal/version/`, `cli/internal/ulid/`, `cli/internal/xdg/`,
`cli/internal/creds/`, `cli/internal/runs/`, `cli/internal/fsx/`,
`cli/internal/harness/`. `harness.Run(t, cmd *cobra.Command, args, stdin, env,
tty bool) (stdout, stderr string, exit int)` **takes a constructed command**;
it does not know the root. Delivers AC5–AC14, AC82, AC84, AC90, AC92.

**U2 — API client and decision table.**
Owns `cli/internal/api/` and `cli/internal/outcome/`. Split into **U2a**
(client, envelope, meta, transport, retry, redaction, outcome table, the AC25
pins that exist at base) and **U2b** (`schema_pin_test.go` and the typed body
structs' final field sets — AC86, after U8). Delivers AC15–AC28, AC71, AC74,
AC83, AC86.

**U3 — Fixture server.**
Owns `cli/internal/fixture/`. Reads `cli/testdata/recorded/`. Delivers
AC29–AC32, AC77, AC91.

**U4 — Auth, keys, corridors, rendering, pre-check.**
Owns `cli/internal/render/`, `cli/internal/noun/auth/`, `cli/internal/noun/keys/`,
`cli/internal/noun/corridors/`, `cli/internal/noun/precheck.go` and its test.
Each noun package exposes `Command(deps) *cobra.Command` and is tested through
`harness.Run` with that command. Delivers AC33–AC40, AC42, AC43.

**U5 — Money path, polling, runs, consent, root.**
Owns `cli/cmd/ferry/main.go`, `cli/internal/cli/` (root, global flags, signal
handling, the in-flight flag, single-document JSON, exit plumbing, `json_test.go`),
`cli/internal/poll/`, `cli/internal/noun/transfers/`, `cli/internal/noun/commandq/`,
`cli/internal/noun/runs/`, `cli/internal/consent/`, `cli/internal/fault/`.
Delivers AC41, AC44–AC59, AC69, AC70, AC72, AC73, AC75, AC78, AC80, AC81,
AC87, AC93, AC94.

**U6 — CI, e2e, release.**
Owns `.github/workflows/ci.yml` (the two jobs and the one step),
`.github/workflows/cli-release.yml`, `cli/.goreleaser.yaml`, `cli/e2e/**`,
`cli/README.md`, `spec/ci/workflow_spec.rb`. Delivers AC60–AC65.
`FERRY_TEST_DATABASE=ferry_test_cli_u6`.

**U7 — Adversarial matrix, audit, documentation.**
Owns `cli/internal/adversarial_test.go`, `docs/loops/cli/MUTATIONS.md`,
`docs/loops/cli/DOC-DEFECTS.md`, `spec/docs/cli_docs_spec.rb`, the CLI
sentences in `docs/api/AGENTS.md` and `README.md`, and §11.2 of this
document. Delivers AC66–AC68, AC89.

**U8 — Contract schemas (Ruby)** *(v2; v2.1, V8)*.
Owns the `Principal`, `ApiKey`, `Simulation`, `Quote`, `Transaction` schemas
and the **seven** response/item references in `docs/api/openapi.yaml`
(`:69`, `:116`, `:134`, `:159`, `:237`, `:319`, and `List.data.items` at
`:651`), and the new examples in `spec/docs/api_docs_spec.rb` (32 examples at
base; U8 adds, edits none). Touches no serializer and no controller: pins
read rendered objects (AC85). Delivers AC85.

### 4.2 Waves

| Wave | Units | Why |
| --- | --- | --- |
| 1 | U0 ∥ U1 ∥ U2a ∥ U8 | Disjoint files. U0 and U8 are Ruby and touch different files. U2a's pins read schemas that exist at base. |
| 1b | U2b (after U8) ∥ U3 (after U0) | U2b pins structs to U8's schemas; U3 loads U0's recordings. Disjoint from each other. |
| 2 | U4 (after U1, U2, U3) | Needs the harness, the client and a server to answer recorded bodies. |
| 3 | U5 (after U1–U4) | Alone: owns `main.go` and the root. |
| 4 | U6 | Alone: edits `ci.yml`. |
| 5 | U7 | Audits everything. |

### 4.3 What must not run in parallel

1. **Nothing runs alongside U6.**
2. **U3 does not start until U0 has committed `cli/testdata/recorded/`**, and
   U2b not until U8 has merged. A unit that hand-writes what it is waiting for
   is the second authority C13 forbids.
3. **U4 and U5 consume `cli/internal/harness/`; neither edits it.**
4. **`cli/go.mod` is frozen after U1.**
5. **U0 and U6 are the only units that run the Ruby suite**, each on its own
   test database. U8 runs only `spec/docs/`.
6. **AC84 runs in every wave.** A file outside `cli/OWNERSHIP` fails the
   wave's `go test ./...`, not U7's audit.

---

## 5. Design

### 5.1 The surface

Strictly `ferry <noun> <verb> [--flags]`.

```
ferry auth       login --api URL [--env sandbox|live] [--token-stdin] [--profile NAME]
                 whoami | logout [--class api_key|pat]
ferry keys       list [--env] [--status active|all] [--limit] [--all]
                 get <id>
                 create --env sandbox|live --name NAME --scopes a,b[,c] [--expires-at RFC3339 | --expires-in 30d] [--login]
                 revoke <id> [--reason TEXT]
ferry corridors  list | get <id>
ferry transfers  create   <body flags | --body FILE|-> [--broadcast] [--yes] [--idempotency-key K] [--no-wait] [--timeout 120s] [--allow-pending]
                 simulate <body flags | --body FILE|->  [--idempotency-key K] [--allow-pending]
                 execute  --plan TOKEN|-                 [--idempotency-key K] [--yes] [--no-wait] [--timeout 120s] [--allow-pending]
ferry commands   get <cmd_id> | watch <cmd_id> [--timeout]
ferry runs       list [--pending] | show <run_id> | resume <run_id> [--yes] | decline <run_id> | prune [--older-than 30d]
ferry version | completion <shell>
```

Global flags: `--output text|json` (default `text`; **never inferred from the
TTY**), `--profile`, `--api URL` (also `FERRY_API_URL`), `--env` (an
assertion, §5.2), `--debug`, `--retry-budget 60s`, `--no-color`/`NO_COLOR`.

**Body flags** build exactly the contract's `TransferRequest`
(`openapi.yaml:737–799`): `--customer` → `customer_id`; `--from-*` →
`source.{type,id,asset,network,blockchain_address,cash_location_id,cash_location_reference}`;
`--to-*` → `destination.{…, account_holder, …}`; `--amount`, `--amount-side` →
`amount.{value, side}` (`value` sent as the string typed); `--sponsor-gas`;
`--metadata k=v`. Only keys the caller set are emitted. The builder serialises
through fixed structs so the same flags always produce the same bytes (AC78);
the CLI does not pre-validate arms, networks or amounts. `--body` forwards a
JSON document byte-for-byte; `--body` plus any field flag is a usage error.

**Reconciling the landing page** (`../landing/a-monograph/index.html:143–150`,
`158`):

| Landing copy | Plan |
| --- | --- |
| `brew install ferry` | `brew install kurenn/tap/ferry` (§5.15; settled, §8 D-3). **Copy is being changed.** |
| `ferry auth login --env sandbox` | Kept, with `--api URL` required (§8 D-4). `--env` asserts the pasted token's environment. There is no login endpoint (§1.2.15). |
| `ferry transfers create --from wlt_… --to ext_… --amount 100.00 --asset usdc` | Kept as `ferry transfers create`, dry-run by default; the snippet needs `--customer`, `--from-type`, `--to-type`, both assets, `--to-network`, `--to-account-holder`. **Copy change required.** |
| `--broadcast` "is the only way money leaves" | Kept exactly (§5.5). |
| `ferry wallets list` | **Not built**; no endpoint (§10.1 row 1). **Copy must drop it.** |

### 5.2 Credentials and profiles

**A file, not the OS keychain.** `$FERRY_HOME/credentials.json`, directory
0700, file 0600, temp+fsync+rename, refused on read if wider than 0600 (C12).
The primary consumer is headless; the keychain is §10.1 row 3.

```json
{
  "schema": 1,
  "profiles": {
    "default": {
      "api_url": "https://ferry.internal.example",
      "api_key": { "token": "ferry_sk_sandbox_…", "token_prefix": "ferry_sk_sandbox_grpQ0HNk", "environment": "sandbox", "principal_id": "key_01j…", "stored_at": "…" },
      "pat":     { "token": "ferry_pat_…",        "token_prefix": "ferry_pat_grpQ0HNk",         "environment": null,      "principal_id": "pat_01j…", "stored_at": "…" }
    }
  }
}
```

**No API URL is compiled in** (§8 D-4). `auth login` requires `--api` or
`FERRY_API_URL` and stores the value on the profile after `GET /v1/me`
succeeds there; every later command uses the profile's `api_url`, and an
explicit `--api`/`FERRY_API_URL` that differs from it is refused before
connecting (C7, AC9). A profile without `api_url` cannot exist, so "neither
set" can only happen at `login`, where it is exit 2 with the message AC35
names.

**One profile, two slots**, and a requirement per command: `keys *` → `PAT`;
`transfers *`, `commands *` → `APIKey`; `corridors *`, `auth whoami` → `Either`
(§1.2.1). A missing slot is refused before any request (C15). Precedence for
the token sent: `FERRY_TOKEN` (ephemeral, never written) > the profile slot.

**`--env` is an assertion.** `auth login --env sandbox` refuses a
`ferry_sk_live_` token; a money command with `--env` refuses a stored key of
another kind before sending (AC34, AC58). With a PAT `--env` is a usage error.

### 5.3 The run ledger — where the idempotency key lives, and what consent is

Every money invocation is a **run**: a ULID and a record at
`$FERRY_HOME/runs/<run_id>.json`, plus a sidecar `<run_id>.lock` — the only
file `flock` is taken on (§1.2.30), never renamed or replaced while the
record exists, deleted only by `prune` under its own lock and after the
record (V5). Three lock idioms and no others: a **holder** (`Open` for a
command or resume) takes `LOCK_EX|LOCK_NB`, retrying for up to 250 ms before
`ErrRunLocked`, and holds through its last `Record`; a **probe** (`Orphans`,
`Scrub`) takes `LOCK_NB` once and releases within the call, so a holder's
retry outlasts it; **prune** takes the lock, unlinks the record, then the
sidecar, then releases.

```json
{
  "schema": 2,
  "run_id": "01JAAAAAAAAAAAAAAAAAAAAAAA",
  "created_at": "…", "cli_version": "0.1.0",
  "profile": "default", "api_url": "https://…", "environment": "sandbox",
  "credential_class": "api_key", "token_prefix": "ferry_sk_sandbox_grpQ0HNk", "principal_id": "key_01j…",
  "argv": ["transfers", "create", "--broadcast", "…"],
  "steps": [
    { "name": "simulate", "operation": "transfers_simulate", "method": "POST", "path": "/v1/transfers/simulate",
      "idempotency_key": "01JAAAAAAAAAAAAAAAAAAAAAAA-simulate",
      "body_sha256": "…", "body": "{\"…verbatim…\"}",
      "state": "pending", "attempts": 1, "response": null, "outcome": null },
    { "name": "execute", "operation": "transfers_execute", "method": "POST", "path": "/v1/transfers",
      "idempotency_key": "01JAAAAAAAAAAAAAAAAAAAAAAA-execute",
      "body_sha256": null, "body": null, "plan": null, "plan_token": null,
      "state": "not_started", "attempts": 0, "response": null, "outcome": null }
  ]
}
```

`body` is a JSON **string** holding the request bytes verbatim, not an embedded
object: `encoding/json` compacts and HTML-escapes a `json.RawMessage` on every
re-marshal, so a record that `Record` rewrites would stop matching its own
`body_sha256` (A310). A body that is not valid UTF-8 is refused at `Begin`,
before anything is sent. A response body is stored the same way under
`response.body` when it is JSON and under `response.body_text` when it is not,
so an HTML `502` — an AC83 case, and a `pending` one, meaning the money may
have moved — is recordable at all (A311).

**Keys.** `<run>-simulate` / `<run>-execute`, pre-minted together before the
first send. A caller-supplied `--idempotency-key` replaces the step's key for
single-step commands; with `--broadcast` it is a usage error (AC87): two steps
need two keys and the caller who wants to name them runs two invocations.

**Step states.**

```
not_started ─┬─(simulate, or execute with consent on the command line)──► pending
             └─(execute, TTY, no --yes)──► awaiting_confirmation ─┬─ y ──► pending
                                                                  └─ n / EOF / signal ──► declined   (terminal; token scrubbed)
             └─(--broadcast simulate left via 202)──► unreachable                                     (terminal; nothing written to it)
pending ──(Record)──► answered ──(class 0/3/4/7/8)──► terminal
                              └──(class 5/6: resumable)
```

`pending` means "`Begin` has run; the request may or may not have left"
(`attempts` is incremented by `Begin`, so `pending` never has `attempts: 0`).
`awaiting_confirmation` is written *before* the prompt text is written;
nothing in that state has consent. `declined` is written on `n`, EOF, or
`SIGINT`/`SIGTERM`/`SIGHUP` while the prompt is up (V4: `SIGHUP` is a closed
terminal, the ordinary way an unattended prompt dies); a `SIGKILL` leaves
`awaiting_confirmation`, which AC72 shows is not resumable into a send
without consent. **A headless machine can always leave that state**: `ferry
runs decline <id>` writes `declined` and scrubs, and is the remediation a
non-TTY `resume` without `--yes` names alongside `resume --yes`. `decline`
refuses a step in `pending` or later — a request that may have been sent
cannot be declined, only resumed — so it can never turn "unknown" into "no".

**The write that matters.** `runs.Begin(step)`: marshal → write `.tmp` →
`fsync` → `rename` → `fsync(dir)` → return; only then `api.Do`. After the
response, `runs.Record` rewrites the same way, stripping `plan.token` from any
simulate body it stores (AC82) — the fresh simulate 201 is the token's only
carrier, and the token's only permitted resting place is the execute step's
`plan_token` field of a `--broadcast` run.

**Consent (C16).** An execute request leaves the process only if this
invocation carried consent: `--yes`; `--broadcast` or `transfers execute` on a
non-TTY (the command is the consent, as the landing page promises); or `y` at a
TTY prompt that displayed the plan. `runs resume` carries no implicit consent:
for an execute step in `awaiting_confirmation` or `pending` it asks on a TTY,
requires `--yes` on a non-TTY, and otherwise exits (1 for
`awaiting_confirmation`, 6 for `pending`) having sent nothing. Resending a
`pending` execute step cannot double-send — that is the API's guarantee — but
re-asking is free and the alternative is a stored state standing in for a
human, which is what CRITIQUE B3 found. Simulate steps are resumed without
asking.

**Resume, in order.** `runs resume <id> [--yes]`:

1. Take `<id>.lock` as a holder (retry ≤ 250 ms); `ErrRunLocked` → exit 1.
2. Compare the profile's `token_prefix` and `principal_id` to the record's;
   mismatch → exit 7, "re-login with `ferry_sk_sandbox_grpQ0HNk…` and resume"
   (C19, AC70). No request has been sent.
3. Find the first step that is `awaiting_confirmation`, `pending`, or
   `answered` with class 5/6.
4. Execute step → consent per C16; then `Begin` (if `awaiting_confirmation`)
   or straight to `Do` with the stored bytes and key (if `pending`/`answered`).
5. Simulate step → `Do` with the stored bytes and key.
6. For a `--broadcast` run whose simulate is terminal 201 with `plan_token`
   stored on the execute step and the execute step `not_started`: consent, then
   `Begin`, then `Do`. If the simulate replayed without a token (the
   `after_simulate_send_before_record` window, AC80) or left via 202 (AC75),
   the execute step is `unreachable` and the run exits 4: re-simulate under a
   new key. Resume never re-simulates itself (C8).

**Why the token is stored at all, and for how long.** A `--broadcast` run owns
two HTTP calls and the token is the only link between them; not storing it
strands a plan on any crash in the inter-step window (AC80 tests that window).
The cost is a bearer authorisation for one priced transfer in a 0600 file next
to the API key that could re-mint it. It is scrubbed when the execute step
reaches `terminal`, `declined` or `unreachable`, and on any `runs` load once
`plan.expires_at + 10m` has passed by the local clock. **Two honest caveats
(CRITIQUE N2):** the load-time scrub is a local-clock decision — it is in the
safe direction (a scrubbed token makes resume exit 4, never sends), and the
ten-minute grace is twice the plan's lifetime so a skewed clock cannot scrub
a live plan; and on a machine where nobody runs `ferry` again the token
persists until they do. Expiry as a *refusal* remains the server's
(§10.2).

**Concurrency.** Distinct invocations mint distinct run ids. Two `resume`s of
one run: the second gets `ErrRunLocked` (AC12, now on the sidecar so it holds
across rewrites). The lock is also what tells C18's orphan check that a
`pending` run belongs to a live process (AC90).

**Fail-closed on orphans (C18).** Before minting a run, a money command lists
runs for the same profile and environment that are unresolved and whose lock
is free, exempting the run a same-body `--idempotency-key` has just matched
(V3; that run *is* the one being resumed). Any other → exit 1, "unresolved run
01J… exists for this profile; `ferry runs resume 01J…`, `ferry runs decline
01J…` if it was never confirmed, or pass `--allow-pending`". This converts the
`until ferry transfers create …; do sleep 5; done` loop from a double-send into
a refusal on iteration 2 (AC81). `--allow-pending` is for callers who run
concurrent transfers and know it.

**Retention.** `runs prune --older-than 30d` removes only terminal records
(`terminal`, `declined`, `unreachable` steps, all scrubbed); an
`awaiting_confirmation` step is not terminal and is left for `decline` (AC57).
Prune holds the sidecar lock while it unlinks record then sidecar (AC92).

**What the record holds after the fact (V6).** The execute step's `body` is
the exact `{"plan_token": "…"}` bytes `Begin` wrote and is never scrubbed:
`body_sha256` is what lets a resume prove it is resending the same request.
So a token that was sent stays in the record until prune, spent or expired;
a token that was declined was never written to `body` and is scrubbed from
`plan_token`, so it leaves the disk. Display redacts both (AC57).

### 5.4 The request path for a money command, in order

1. Parse flags. Usage error → exit 2. `--idempotency-key` + `--broadcast` →
   exit 2 (AC87). *(Local.)*
2. Resolve profile and credential for `APIKey`. Missing → 3 (AC43). `--env`
   mismatch → 3 (AC58). `--api` differs from profile → 3 (AC9). *(Local.)*
3. Build the body or take `--body` bytes; compute `body_sha256`.
4. If `--idempotency-key`: `FindByKey`; different digest → 3 (AC54); same →
   **route into the resume order of §5.3 for that run** (consent per C16),
   and that run is exempt from step 5 (V3, AC94).
5. Orphan check (C18) unless `--allow-pending` → 1 (AC81). *(Local.)*
6. Mint the run id; pre-mint every step key the run needs (AC47).
7. Take `<run>.lock` as a holder. Write the run record with all steps
   `not_started`.
8. **Execute step only, TTY, no `--yes`**: write `awaiting_confirmation`;
   write the prompt; read. `n`/EOF/`SIGINT`/`SIGTERM`/`SIGHUP` → write
   `declined`, scrub, exit 1. (For `--broadcast` this happens after step 12
   for the simulate.)
9. **`runs.Begin` — write, fsync, rename** (C1); `attempts += 1`. Failure →
   exit 1, nothing sent.
10. **Set the in-flight flag** (C17) — monotonic; nothing ever clears it.
    `api.Do`. Retry policy (§5.8) inside `Do`, never changing key or bytes.
11. `outcome.Classify`. `runs.Record`. Any abnormal exit from here to the end
    of the process — `Record` error, panic, signal — is exit 6 with the run
    id (AC69). *(v2.1: the flag is no longer cleared here — V1.)*
12. If `202` / `UPSTREAM_BUSY`-with-command / `IN_PROGRESS`: `poll.Watch`
    (§5.7) unless `--no-wait`; record the terminal state. A signal during the
    poll is exit 6 (AC69(d)). **`--broadcast` and the step was simulate and it
    left via 202**: write the execute step `unreachable`; after polling, exit
    4 (AC75).
13. **`--broadcast`, simulate 201 fresh**: record `plan{id, expires_at}` and
    `plan_token` on the execute step; then steps 8–12 for the execute step.
    Simulate 201 replayed (no token): execute `unreachable`, exit 4.
14. Render (§5.9); **the normal path exits with the class's code** whatever
    the flag says; a renderer panic is abnormal and, with the flag set, exit 6.

Steps 1–6 send nothing and write nothing to the ledger; step 7 is the first
durable effect; step 9 precedes step 10. Every abnormal exit after step 10 is
reported as 6 for the rest of the process's life.

### 5.5 `--broadcast`

`ferry transfers create` is a **simulate**: it prices, prints the quote, prints
the plan token once with its `expires_at` and remaining time, and exits 0.

`--broadcast` means: **simulate, then execute the plan this invocation just
received, in this process.** Two HTTP calls, two keys minted before the first
call, one run record.

On a TTY without `--yes` the CLI shows both sides, fees, corridor, environment
(`LIVE` in capitals if live) and `plan expires at … (4m58s)`, and asks
`Execute? [y/N]`. A human who takes six minutes gets `410 PLAN_EXPIRED`, exit
4, nothing moved, and is told to re-simulate; the CLI does not do it for them
(C8). On a non-TTY there is no prompt: `--broadcast` is the consent, and an
agent that wants a human in the loop runs `create` and `execute` as two
invocations. A simulate that answers `202` under `--broadcast` cannot lead to
an execute — the token was never issued (§1.2.9) — and the run exits 4 after
polling (AC75).

`ferry transfers execute --plan <token|->` is the second half on its own and
is C16's third arm (AC93). On a non-TTY the command is consent; on a TTY
without `--yes` it writes `awaiting_confirmation`, prompts showing what it can
(the token's prefix, the environment) since it holds no quote, and writes
`declined` on `n`.

### 5.6 The decision table, as behaviour

`internal/outcome` maps every answer to `Outcome{Class, Exit, Money,
SameKeySafe, Next}` from the tuple in C4.

**Outcome classes and exit codes:**

| Exit | Class | Meaning | Money? | What the caller does |
| --- | --- | --- | --- | --- |
| 0 | `done` / `accepted_upstream` | A read succeeded; a simulate returned a plan; an execute was **accepted upstream** | no / `accepted_upstream` | For execute: read `status`; reconcile with Polygon later. |
| 1 | `cli_fault` | The CLI stopped **before any money request left**: config, I/O, lock, orphan check, user decline, bug before the wire | no | Fix the local problem, or `runs resume` the named run. **Never emitted once the in-flight flag is set (C17).** |
| 2 | `usage` | Flags or arguments were wrong | no | Fix the invocation. |
| 3 | `refused_fix` | Refused; nothing sent; nothing spent | no | Fix the request or credential; **new** key. |
| 4 | `refused_resimulate` | Refused or unusable; nothing sent; the plan is spent, rolled back, or was never issued | no | Re-simulate; execute under a new key. |
| 5 | `transient` | Refused transiently; nothing sent | no | Resend the **same** key after `retry_after_seconds`. |
| 6 | `pending` | Outcome not established — including every post-send fault | **unknown** | `ferry runs resume <id>` / `ferry commands watch <cmd>`. **Never a new key.** |
| 7 | `escalate` | FERRY cannot or will not resolve this; or the CLI lacks the credential to find out | **unknown** | Stop. A human reads the command. |
| 8 | `upstream_failed` | Accepted upstream, then `status: failed` | see upstream | Reconcile against Polygon; new simulate. |

**Money operations** (`transfers_simulate`, `transfers_execute`):

| Answer | Class / exit | Notes |
| --- | --- | --- |
| `201` simulate, fresh | `done` 0 | Token printed once. |
| `201` simulate, `Idempotency-Replayed: true` | `done` 0 + warning | No token; `meta.remediation` rendered. Under `--broadcast`: execute `unreachable`, run exits 4. |
| `201` execute, `status ≠ failed` | `accepted_upstream` 0 | |
| `201` execute, `status == failed` | `upstream_failed` 8 | |
| `202` | `pending` → §5.7 | Terminal classification by AC74. |
| `400 *`, `401 *` | `refused_fix` 3 | Plan untouched (§1.2.4). |
| `403 WRONG_TOKEN_CLASS`, `INSUFFICIENT_SCOPE`, `ROLE_FORBIDDEN`, `STEP_UP_REQUIRED`, `FORBIDDEN` | `refused_fix` 3 | |
| `403 EXECUTION_SUSPENDED` on simulate | `refused_fix` 3 | No plan exists to spend. |
| `403 POLICY_*`, `EXECUTION_SUSPENDED`, `DESTINATION_TOO_NEW` on execute | `refused_resimulate` 4 | `Denied`: plan consumed. `DESTINATION_TOO_NEW` is unreachable today (§1.2.7) but mapped. |
| `403 PLAN_KEY_MISMATCH` | `refused_resimulate` 4 | **Rolled back, not spent** (`begin.rb:47–51`); the remedy is still a new plan under the right key. *(Sentence corrected, CRITIQUE.)* |
| `404 PLAN_NOT_FOUND` | `refused_resimulate` 4 | |
| `404 NOT_FOUND` | `refused_fix` 3 | |
| `409 IDEMPOTENCY_KEY_IN_PROGRESS` | `pending` 6 → poll `details.command_id` | Never a new key (AC53). |
| `409 IDEMPOTENCY_KEY_REUSED`, `details.reason == "different_request"` | `refused_fix` 3 | A client bug; the owning local run is named if any. |
| `409 IDEMPOTENCY_KEY_REUSED`, `reason == "different_credential"` or absent | `escalate` 7 | **Something of yours may be under this key and you no longer hold the credential to read it** (§1.2.25). `next` names `details.command_id` and the credential prefix from the run record. |
| `409 IDEMPOTENCY_KEY_REPLAY_EXPIRED` | `escalate` 7 | `details.state` rendered ("the command under this key is `completed`"). |
| `409 COMMAND_UNRESOLVED` | `escalate` 7 | |
| `409 PLAN_ALREADY_USED`, `PLAN_CHANGED` | `refused_resimulate` 4 | |
| `410 PLAN_EXPIRED` | `refused_resimulate` 4 | |
| `422 *` (simulate) | `refused_fix` 3 | |
| `429 RATE_LIMITED` | `transient` 5 after budget | |
| `500 INTERNAL_ERROR` | `pending` 6 after budget | |
| `502 UPSTREAM_CONTRACT_VIOLATION` execute / simulate | 4 / 3 | Pre-send on execute (§1.2.5). |
| `503 UPSTREAM_BUSY`, no `Ferry-Command-Id` | `transient` 5 after budget | |
| `503 UPSTREAM_BUSY`, with `Ferry-Command-Id` | `pending` 6 → same key after interval → `202` → poll | |
| `503 UPSTREAM_UNAVAILABLE` | `refused_resimulate` 4 | Only ever replayed; plan spent. |
| `503 UPSTREAM_NOT_CONFIGURED` | `refused_fix` 3 | |
| `503 UPSTREAM_CREDENTIALS_INVALID` | `refused_fix` 3 | Unreachable; mapped. |
| `503 SERVICE_UNAVAILABLE`, `UPSTREAM_AUTH_UNAVAILABLE` | `transient` 5 after budget | |
| Any status with a body that is not the seven-key envelope; any status with no row (`413`, `415`, HTML `502`, …) | `pending` 6 | C3; AC83. |
| Dial error | `transient` 5 | Nothing left the host. |
| Any error after the request was written; unparseable 2xx | `pending` 6 | C3. |
| Unknown code | `pending` 6 | C3, AC24. |

**Reads (`GET`)**: 2xx → 0; 400/401/403/404 → 3; 429/5xx/transport → 5 after
budget; non-envelope or unlisted → 5; unknown code → 3 for 4xx, 5 for 5xx.
**`control_create`**: as reads for refusals; never retried; transport after
write → 6 with "run `ferry keys list`". **`control_revoke`**: idempotent,
retried like a read. **`599` from the fixture**: never retried, `cli_fault` 1
in tests.

**Terminal command states** (AC74), selected by the body's `operation`:

| `operation` | `state` | Rule |
| --- | --- | --- |
| any | `contradiction != null` | `escalate` 7 |
| any | `needs_operator` | `escalate` 7 |
| `transfers_execute` | `completed`, `result != null` | classify `result.body.status` as a 201: 0 or 8 |
| `transfers_execute` | `completed`, `result == null` | 0, "result no longer replayable; `transaction_id` = …" |
| `transfers_simulate` | `completed` | `refused_resimulate` 4: "the plan token was never issued — simulate again under a new key" |
| any | `failed_terminal` | the row for `last_error.code` above; for a transfer typically 4 |

### 5.7 Polling

One loop, `poll.Watch(cmdID, deadline)` (AC59):

1. `GET /v1/commands/{id}`.
2. `contradiction != null` → 7. 3. `completed`/`failed_terminal` → AC74.
4. `needs_operator` → 7. 5. otherwise sleep `max(retry_after_seconds, 1)`
(`nil` → 5), print the transition to stderr in text mode, loop. 6. Deadline →
6 with the command id.

`--timeout` default 120 s; `--no-wait` prints the 202 and exits 6. `poll` is a
path joined to the profile's `api_url`; an absolute URL there is refused
(CS-13).

### 5.8 Retry policy

Inside `api.Do`: wait `max(retry_after_seconds, 1)`, resend the **same bytes
and headers**, repeat within `--retry-budget`. The key is fixed before the
loop, which has no access to the minter (AC27). `control_create`: none. `599`:
none. Nothing else is retried automatically.

### 5.9 Output modes and rendering

`--output json` writes **one** document to stdout on every exit path (C10,
AC41): the root installs a single top-level renderer; the signal handler and
panic recovery route through it and consult the monotonic in-flight flag
(C17) to choose between `cli_fault`/1 (never set in this process) and
`pending`/6 (set at any point in this process, whether or not `Record` has
since succeeded).

```json
{
  "ferry_cli": { "version": "0.1.0", "run_id": "01J…", "profile": "default", "environment": "sandbox" },
  "outcome":   { "class": "accepted_upstream", "exit_code": 0, "money": "accepted_upstream",
                 "same_key_safe": true, "next": "…", "warnings": [] },
  "http":      { "status": 201, "request_id": "req_…", "command_id": "cmd_…", "idempotency_key": "01J…-execute",
                 "replayed": false, "environment": "sandbox", "retry_after_seconds": null },
  "response":  { "…API body, verbatim…" },
  "error":     null
}
```

Text mode for a money command always prints the same block (run, outcome,
status, txn/command, next); `LIVE` prefixes every line for a live credential;
"success" and "✓" never appear in the transfers renderer (AC49). Secrets are
printed in exactly two renders (AC42).

### 5.10 Recorded interactions

**What a recording is the authority for, and what it is not (CRITIQUE M4,
"weakest assumption").** The recorder drives the real Rack app; its *response*
bodies are authoritative for shape and content, and its *request* bodies are
authoritative for **semantic content** — which keys, which values — but not
for bytes. Ruby's `JSON.generate` and Go's `encoding/json` do not agree on key
order, and pinning one to the other would couple two serialisers for a
property the server does not care about: the server's digest matters only
when the *same client* resends, which is Go-vs-Go and is asserted at the
fixture's request log (AC45, AC31), never against the recording. So:

- The fixture matches a request on method, path, `Idempotency-Key` presence,
  and **structural JSON equality** of the body after placeholder substitution
  (AC30). Extra, missing or different keys/values are unrecorded; order and
  whitespace are not.
- **Byte identity across a resume** is asserted Go-vs-Go at the fixture's
  request log (AC45, AC80).
- **`--body` forwarding byte-for-byte** is asserted input-vs-wire with a
  non-canonical document as input (AC56).
- **That the Rails app accepts Go's serialisation** is asserted only by e2e
  (AC61–AC63). Ordering and whitespace are not semantic to
  `request.request_parameters`, so this is a low-risk gap, and it is named.
- Recordings carry no `body_sha256`; the placeholder token
  `ferry_plan_PLACEHOLDER` is what the fixture hands out in `simulate.201` and
  what it expects back in `execute.*`.

**What becomes untested by this choice:** nothing on the money path that the
fixture previously claimed to test; what is lost is a byte-level claim the
recording could never honestly make. The decision table (AC19–AC24, AC71,
AC74, AC83) is pure and consumes no recordings.

**Format.** One file per scenario, plus `MANIFEST.json` with `{scenario, sha256,
expect}`:

```json
{
  "scenario": "execute.policy_denied.403",
  "expect": [ { "status": 403, "code": "/^POLICY_/", "headers_present": ["Ferry-Command-Id"], "body_has": ["error.details"], "body_equals": {} } ],
  "interactions": [
    { "request":  { "method": "POST", "path": "/v1/transfers",
                    "headers": { "idempotency_key_present": true, "credential_class": "api_key" },
                    "body": { "plan_token": "ferry_plan_PLACEHOLDER" } },
      "response": { "status": 403,
                    "headers": { "Ferry-Command-Id": "cmd_PLACEHOLDER_1", "Idempotency-Key": "IDEMPOTENCY_KEY_PLACEHOLDER_1", "Ferry-Environment": "sandbox" },
                    "body": { "error": { "code": "POLICY_MAX_AMOUNT_PER_TRANSACTION", "…": "…" } } } }
  ]
}
```

The recorder asserts `expect` against the Rack app's answer before writing
(AC76); the Go loader asserts it again against the file (AC77). A recorder
that captures the wrong thing under a name is red on the Ruby side.

**Scenario set** (AC1; the manifest equals this list):

- Identity/auth: `me.api_key.200`, `me.pat.200`, `auth.token_missing.401`,
  `auth.token_invalid.401`, `auth.token_revoked.401`,
  `auth.wrong_class.api_keys_with_api_key.403`,
  `auth.wrong_class.transfers_with_pat.403`, `auth.insufficient_scope.403`,
  `auth.unsupported_media_type.415`.
- Keys: `keys.list.page1.200`, `keys.list.page2.200`, `keys.list.bad_status.400`,
  `keys.create.sandbox.201`, `keys.create.live_money.step_up.403`,
  `keys.create.unknown_key.400`, `keys.create.expires_beyond_ceiling.400`,
  `keys.get.200`, `keys.get.404`, `keys.revoke.200`, `keys.revoke.again.200`.
- Corridors: `corridors.list.200`, `corridors.get.walletCrypto_bankUs.200`,
  `corridors.get.walletCrypto_cash.200`, `corridors.get.unknown.404`.
- Simulate: `simulate.201`, `simulate.replay.201`, `simulate.202.then_completed`
  (sequence: 202, poll `upstream_unknown`, poll `completed` — `result.body`
  has no `token` and no `status`), `simulate.in_progress.409`,
  `simulate.key_required.400`, `simulate.unknown_key.400`,
  `simulate.corridor_unsupported.422`, `simulate.amount_invalid.422`,
  `simulate.execution_suspended.403`, `simulate.upstream_not_configured.503`,
  `simulate.rate_limited.429`, `simulate.quote_rejected.422`,
  `simulate.contract_violation.502`.
- Execute: `execute.201.processing`, `execute.201.failed_status`,
  `execute.replay.201`, `execute.202.then_completed`,
  `execute.202.then_needs_operator`, `execute.202.replayed`,
  `execute.in_progress.409`, `execute.key_reused.different_request.409`,
  **`execute.key_reused.different_credential.409`** (mint a second, unlinked
  sandbox key; replay under it), `execute.replay_expired.409` (via
  `expire_replay_window!`; `details.state` present), `execute.command_unresolved.409`,
  `execute.upstream_busy.with_command.503` (sequence), `execute.upstream_busy.no_command.503`,
  `execute.policy_denied.403`, `execute.execution_suspended.403`,
  `execute.plan_key_mismatch.403`, `execute.plan_already_used.409`,
  `execute.plan_changed.409`, `execute.plan_expired.410`, `execute.plan_not_found.404`,
  `execute.contract_violation.502`, `execute.upstream_unavailable.503`,
  `execute.upstream_not_configured.503`, `execute.service_unavailable.503`,
  `execute.internal_error.500`.
- Commands: `commands.get.<state>.200` × 7, `commands.get.completed.contradiction.200`,
  `commands.get.failed_terminal.contradiction.200`, `commands.get.404`.

**Unreachable, not recorded (CS-7, CRITIQUE M6):** `execute.destination_too_new.403`
— no HTTP path supplies `destination_usable_after` (§1.2.7). The Go row is
asserted from the table alone (AC19, AC20) and the code is marked `unreachable`
in the table. Also unreachable and not recorded: `UPSTREAM_CREDENTIALS_INVALID`,
`UPSTREAM_AUTH_UNAVAILABLE`.

Rows the scripted gateway cannot reach from the request path
(`needs_operator`, `contradiction`, `UPSTREAM_UNAVAILABLE`, `REPLAY_EXPIRED`,
and `UPSTREAM_CONTRACT_VIOLATION` — A390) are recorded by building the command
row with the ledger builders and driving the HTTP request that reads or
replays it. Every response body in the directory came out of the Rack app.

A recording taken this way is **evidence about the replay path, not the live
one**, and the manifest records its provenance. Its `Idempotency-Replayed` and
`Ferry-Command-Id` headers describe the seeded row rather than a live send, so
they must not be read as evidence about whether a plan was consumed — a
reading that nearly produced a false finding against §1.2.5 (A391).

### 5.11 The fixture server

`internal/fixture.New(t, recordings...)` returns an `httptest.Server` that
matches per AC30, plays sequences in order (AC31), logs every request with raw
bytes and sha256, can hold a request open until released (AC91), answers
anything unmatched with `599` (AC29), sets only recorded headers (AC32), and
refuses a directory whose files disagree with the manifest's `expect` (AC77).

### 5.12 Contract pinning — and fixing the contract

**The contract is fixed, not worked around (CRITIQUE, "type: object").** U8
adds `Principal`, `ApiKey`, `Simulation` (with nested `Quote`, `Plan` and a
replay-only `Meta`) and `Transaction` schemas to `openapi.yaml`, points the
**seven** untyped bodies at them (six responses plus `List.data.items`, V8),
and pins each schema's property set to rendered output of the serializer or
allowlist that produces the body in `spec/docs/api_docs_spec.rb` (AC85) — the
same mechanism that already pins `TransferRequest` and `CreateApiKeyRequest`
(§1.2.28). Conditional properties are three and are documented as such:
`ApiKey.token` (create 201 only), `Simulation.plan.token` (fresh 201 only),
`Simulation.meta.remediation` (replayed 201 only — `command_request.rb:331`;
V2). Cost: roughly 180 lines of YAML and six or seven spec examples; no
serializer changes. The Rails side then owns the type authority for every
body the CLI reads, and recordings test behaviour, not shape.

**A393: the seven were the *response bodies*, not every `type: object` node.**
Four object nodes inside them stayed bare — `Error.error.details`,
`Command.result.body` (U2b's A321), `Command.last_error` and
`Command.contradiction` — and all four are load-bearing: C19 splits on
`details.reason`, AC71 polls `details.command_id`, AC74 decides on
`result.body.status`, and AC21 escalates on `contradiction`. A393 types them
as `ErrorDetails` (open, with the emitted vocabulary swept out of the Ruby
emitters and held equal in both directions), a `oneOf` union of `Transaction`
and a new `StoredSimulation`, an open `last_error`, and a closed
`contradiction`. The union made `oneOf` reachable by the Go reader, which U2b
had left fatal; `schema_pin_test.go` now resolves it into arms and pins each.

Three pins on the Go side:

1. **Structure from `openapi.yaml`**: paths and methods, the `IdempotencyKey`
   parameter, `Error.required`, `Command.state`, request property sets, the
   `scopes` enum (AC16, AC17, AC25), **and the five response schemas**
   (AC86, U2b).
2. **Behaviour from recordings** with declared expectations (AC76, AC77).
3. **Codes from `errors.md`** (AC19), with "Codes you will not see" marked
   `unreachable`.

**Could the client then be generated?** Yes, for types: with the schemas in
place `oapi-codegen -generate types` would emit the same structs AC86 pins,
and the swap would cost a build step, a tool dependency, and generated code
for nine routes that are trivial by hand. What generation would *not* produce
is the product: the decision table, the run ledger, consent, polling. v2
therefore hand-writes the structs and pins them field-for-field to the schemas
(so a later switch to generated types is a mechanical replacement with the
same test), and records the choice as WD1. If the contract grows past a dozen
routes, generate the types.

What breaks when the contract changes: a new route reddens AC25; a new code
reddens AC19; a changed body reddens AC85 on the Rails side *first* (the
serializer and the schema disagree), then the recorder's `git diff`, then
AC86.

### 5.13 Repository layout, module, dependencies

Its own repository, `kurenn/ferry-cli`, module `github.com/kurenn/ferry-cli` (§8 D-2, amended by A400).

```
cli/
  go.mod  go.sum  OWNERSHIP  ownership_test.go        U1
  cmd/ferry/main.go                                   U5
  internal/cli/          root, global flags, signal handler, in-flight flag, single-document JSON, exit plumbing, json_test.go   U5
  internal/version/  ulid/  xdg/  fsx/  creds/  runs/  harness/                                                                  U1
  internal/api/  outcome/                             U2
  internal/fixture/                                   U3
  internal/render/  noun/precheck.go  noun/auth/  noun/keys/  noun/corridors/                                                     U4
  internal/poll/  consent/  noun/transfers/  noun/commandq/  noun/runs/  fault/                                                    U5
  internal/adversarial_test.go                        U7
  e2e/                                                U6
  testdata/recorded/                                  U0
  .goreleaser.yaml  README.md                         U6
```

**Dependencies, fixed**: `github.com/spf13/cobra` (+`pflag`); `gopkg.in/yaml.v3`
(tests only); `golang.org/x/term` (TTY detection, no-echo prompt). No CLI code
imports `golang.org/x/sys` — the lock is `syscall.Flock` (§1.2.29; CRITIQUE
N7) — but it sits in the module graph as an **indirect** dependency of `x/term`,
which cannot be avoided while `x/term` is required (A313). `x/term` is pinned at
`v0.40.0`: `v0.41.0+` needs Go ≥ 1.26 and `go.mod` declares `go 1.24.0`, so a
bump moves the toolchain floor. `pflag` is `// indirect` and a later unit may
import it directly, but **no unit after U1 runs `go mod tidy`** — it would
reclassify `pflag` and so edit a file §4.3.4 freezes after U1. Nothing else.

**Platforms**: `darwin/amd64`, `darwin/arm64`, `linux/amd64`, `linux/arm64`.

### 5.14 CI

Three changes to `ci.yml`. The `cli` job lands **with U1**, not with U6 as
revision 2 had it (A315): giving it to U6 meant U1–U5 would each merge Go that
no automated gate had run, on hand-verification alone.

- **`test`**: one added step, `git diff --exit-code cli/testdata/recorded` —
  landed at U0, not U6 (A306), for A315's reason: the recordings arrive with
  U0, and until the step exists nothing checks they reproduce on a machine
  other than the one that recorded them.
- **`cli`** (`timeout-minutes: 10`): landed at U1 — `actions/setup-go` from
  `cli/go.mod`; `gofmt -l` empty; `go vet ./...`; `go test -count=1 -race
  ./...`; `git diff --exit-code go.mod go.sum` (the freeze in §4.3.4 and A313).
  U6 adds the steps whose tooling it introduces: `go test -count=1 -tags
  faultinject ./internal/noun/transfers/... ./internal/noun/runs/...`,
  `goreleaser check`, `goreleaser build --snapshot --clean`.
- **`cli-e2e`** (`timeout-minutes: 20`, Postgres 18 + Redis 8): prepare the
  database as `test` does; bootstrap with `RAILS_ENV=test bin/rails runner
  cli/e2e/bootstrap.rb` (creates a `User`, calls `Organizations::Provision`,
  shells out to `bin/rails "ferry:pat:issue[…]"` and parses the `ferry_pat_`
  line); start `RAILS_ENV=test FERRY_OMS_BACKEND=fixture bin/rails server -p
  3000`, wait for `/up`; build the release-shaped and `faultinject` binaries;
  `go test -tags e2e ./e2e/...` with `FERRY_API_URL=http://127.0.0.1:3000`,
  `FERRY_E2E_PAT`, `FERRY_E2E_BIN`, `FERRY_E2E_FAULT_BIN`. Server killed in an
  `always()` step.

`bootstrap.rb` refuses `Rails.env.production?` and any `FERRY_OMS_BACKEND` other
than `fixture`/`mock`.

### 5.15 Versioning and release

- **Tags** `cli/v<semver>`. `ferry version` prints version, commit, Go version,
  and the `openapi.yaml` sha256 embedded at build; the same goes on the wire in
  `User-Agent`. No server handshake exists (WD3).
- **Build**: goreleaser, `CGO_ENABLED=0`, `-trimpath`, `-ldflags "-s -w -X …"`
  (no API URL is injected — §8 D-4), four targets, `checksums.txt`, GitHub
  Release on tag (`cli-release.yml`, `timeout-minutes: 15`).
- **Homebrew**: goreleaser's `brews` block pushes `Formula/ferry.rb` to
  `kurenn/homebrew-tap` with `HOMEBREW_TAP_GITHUB_TOKEN`. Install: `brew install
  kurenn/tap/ferry`.
- **Signing**: §10.1 row 7.

### 5.16 What the CLI can do today

Against the deployment, `transfers create` exits 3 with `UPSTREAM_NOT_CONFIGURED`
and "nothing to retry until an operator attaches a credential". Against a
FERRY with `FERRY_OMS_BACKEND=fixture` it reaches 201 on both steps (AC61).

---

## 6. Verification

### 6.1 How this slice is tested

**Pure Go (U1, U2).** ULID, paths, credential store, run ledger against a
fault-injecting filesystem, the outcome table (now with `details.reason` and
`operation` as inputs), transport classification, redaction.

**Contract (U8 → U2b).** Rails pins the five response schemas to their
serializers; Go pins its structs to the schemas.

**Recorded interactions (U0 → U3 → U4, U5).** Real Rack app, declared
expectations asserted on both sides, structural request matching.

**Process-level (U5).** The `faultinject` build tag compiles `fault.Point`
into the binary. Fault points: `after_record_written`,
`after_send_before_record`, `record_fails_after_send`,
`render_panics_after_201` (fires after `Record` returned nil, inside the
renderer), `at_prompt` (fires after the prompt text is written, before the
read), `after_simulate_recorded_before_execute_begin`,
`after_simulate_send_before_record`. Plus SIGINT delivered while the fixture
holds a request open (AC91) and during `poll.Watch` (AC69(d)), and SIGHUP to
a pty at the prompt (AC72). Every fault-point test runs with `-count=1`.

**End to end (U6).** Real Rails app with the fixture backend; one happy path,
crash-resume at two points, three refusals.

Structural learnings applied: vocabularies asserted by parsing both
directions; detectors proven able to fire (redaction and secret canaries;
`599` exercised; `expect` mismatches exercised); controls exercise the system
(processes killed, bytes counted at the server); nothing inferred from the TTY
changes bytes on the wire or the stdout format.

### 6.2 Mutation table

Every control names the change that must break it. Rows M1–M62 are revision
1's (amended where the AC changed); M63 onward are revision 2's. §11.2 records
what was observed.

| # | Mutation | Example that must fail | Assertion |
| --- | --- | --- | --- |
| M1 | Send before `runs.Begin` | `crash_test.go` | fixture count 0, record present (AC44) |
| M2 | Rename before `fsync` | `ledger_test.go` | syscall order (AC10) |
| M3 | Mint a fresh key on `resume` | `crash_test.go` | same key both requests (AC45); e2e: two `Ferry-Command-Id`s (AC62) |
| M4 | Recompute `body_sha256` instead of comparing | `ledger_test.go` | `ErrRecordCorrupt` (AC11) |
| M5 | Lock `<run>.json` instead of the sidecar | `ledger_test.go` "refused after a rewrite" | B's `Open` succeeds (AC12) |
| M6 | Never scrub | `ledger_test.go` | token present (AC13) |
| M7 | Scrub `body_sha256` too | same | digest changed (AC13) |
| M8 | `FindByKey` by prefix | `ledger_test.go` | near-miss found (AC14) |
| M9 | `0755` directories | `paths_test.go` | (AC6) |
| M10 | Drop the too-wide check | `store_test.go` | (AC7) |
| M11 | Return the PAT for `APIKey` | `store_test.go` | (AC8) |
| M12 | Ignore `api_url` in `For` | `store_test.go` | `ErrAPIURLMismatch` absent (AC9) |
| M13 | Re-marshal the body before sending | `client_test.go` | byte inequality (AC15) |
| M14 | `Idempotency-Key` on `GET` | `contract_test.go` | (AC16) |
| M15 | Accept a six-key envelope | `contract_test.go` | (AC17) |
| M16 | Absent `Retry-After` → `0` | `meta_test.go` | (AC18) |
| M17 | Delete the `DESTINATION_TOO_NEW` row | `coverage_test.go` | unmapped (AC19) |
| M18 | Add a `FOO_BAR` row | `coverage_test.go` | phantom (AC19) |
| M19 | `POLICY_*` on execute → 3 | `table_test.go` | (AC20) |
| M20 | `UPSTREAM_UNAVAILABLE` → `transient` | `table_test.go` | (AC20) |
| M21 | `UPSTREAM_BUSY`-with-command → 5 | `table_test.go` | (AC20) |
| M22 | `state` before `contradiction` | `table_test.go` | (AC21) |
| M23 | Every 201 → `done` | `table_test.go` | 8 on `failed` (AC22) |
| M24 | All transport errors → 5 | `transport_test.go` | (AC23) |
| M25 | Unknown money code → 3 | `table_test.go` | 6 (AC24) |
| M26 | Add a `wallets` operation | `contract_test.go` | (AC25) |
| M27 | `allowed_assets` as `[]string` | `corridor_test.go` | (AC26) |
| M28 | New key inside the retry loop | `retry_test.go` | (AC27) |
| M29 | Retry `control_create` | `retry_test.go` | count 2 (AC27, AC38) |
| M30 | Log `Authorization` raw | `redact_test.go` | (AC28) |
| M31 | `200 {}` for unrecorded | `server_test.go` | 599 expected (AC29) |
| M32 | Match money POSTs on path alone | `server_test.go` "extra key is unrecorded" | 599 expected (AC30) |
| M33 | Always serve the first interaction | `poll_test.go` | never `completed` (AC31, AC50) |
| M34 | Invent `Ferry-Command-Id` | `server_test.go` | (AC32) |
| M35 | Add `--token` | `login_test.go` | (AC33) |
| M36 | Ignore `--env` on login | `login_test.go` | (AC34) |
| M37 | Store before `GET /v1/me` | `login_test.go` | (AC35) |
| M38 | Print the token in `whoami` | `secrets_test.go` | (AC36, AC42) |
| M39 | Send `"expires_at": null` | `create_test.go` | (AC37) |
| M40 | Construct `cursor` | `list_test.go` | (AC39) |
| M41 | Skip encoding `->`, asserted on `api.Expand`'s **return value**, not over HTTP (A332) | `get_test.go` | (AC40) |
| M42 | Usage error to stderr only in JSON mode | `internal/cli/json_test.go` | stdout empty (AC41) |
| M43 | Send when the class is missing | `precheck_test.go` | (AC43) |
| M44 | Execute without `--broadcast` | `create_test.go` | count 2 (AC46) |
| M45 | Store the token on a simulate-only run | `create_test.go` | (AC46) |
| M46 | Re-simulate on `PLAN_EXPIRED` | `broadcast_test.go` | simulate count 2 (AC47) |
| M47 | Mint the execute key after the simulate 201 | `broadcast_test.go` | (AC47) |
| M48 | Prompt on a non-TTY | `broadcast_test.go` "no prompt text, execute sent" | prompt text on stderr / execute count 0 (AC48) *(v2: no timeout detector, CRITIQUE N6)* |
| M49 | Print `✓ success` | `render_test.go` | (AC49) |
| M50 | `time.Sleep(0)` in the poll loop | `poll_test.go` | sleeper calls ≠ `[5,5,1]` (AC50) |
| M51 | Keep polling on `needs_operator` | `poll_test.go` | (AC51) |
| M52 | Resend `UPSTREAM_BUSY`-with-command under a new key | `busy_test.go` | (AC52) |
| M53 | Skip `FindByKey` | `userkey_test.go` | (AC54) |
| M54 | Add `"broadcast": true` to the execute body | `execute_test.go` | 599 (AC55) |
| M55 | Canonicalise `--body` | `body_test.go` | whitespace normalised (AC56) |
| M56 | `prune` removes pending | `runs_test.go` | (AC57) |
| M57 | Drop the `--env` check | `env_test.go` | (AC58) |
| M58 | Second poll loop | `sweep_test.go` | (AC59) |
| M59 | Remove `timeout-minutes` from `cli` | `workflow_spec.rb` | (AC60) |
| M60 | e2e scopes `read,money:simulate` | `happy_test.go` | 403 (AC61) |
| M61 | Drop `-X main.version` | `release_test.go` | (AC64) |
| M62 | Delete a §7.1 row | `adversarial_test.go` | (AC66) |
| M63 | Remove a scenario from the manifest only | `recorded_interactions_spec.rb` | manifest ≠ scenario list (AC1) |
| M64 | Drop the `request_id` substitution | same, "byte-stable" | second recording differs (AC2) |
| M65 | Drop `Ferry-Command-Id` from the captured header subset | same | key-set inequality (AC3) |
| M66 | Write recordings to `tmp/` | same + CI | committed directory unchanged where it should differ; `git diff` clean when dirty expected (AC4) |
| M67 | Replace `crypto/rand` with `math/rand` seeded 0 | `ulid_test.go` | collision across two generators (AC5) |
| M68 | Resend `IN_PROGRESS` under a fresh key | `busy_test.go` | key inequality / POST count 2 (AC53) |
| M69 | At `after_record_written`, assert `Idempotency-Replayed` present | `resume_test.go` (e2e) | the AC's first branch is asserted absent there (AC62) |
| M70 | Make `--no-precheck` the default | `refusals_test.go` (e2e) | request count 1 where 0 expected (AC63) |
| M71 | Trigger `cli-release.yml` on `v*` | `workflow_spec.rb` | (AC65) |
| M72 | Restore "no CLI … neither exists" to `AGENTS.md` | `cli_docs_spec.rb` | (AC67) |
| M73 | Post-send SIGINT → exit 1 | `postsend_test.go` (a) | exit 6 expected (AC69) |
| M74 | `Record` error → exit 1 | `postsend_test.go` (b) | exit 6, `pending` (AC69) |
| M75 | Clear the in-flight flag when `Record` returns nil (v2's own step 11) | `postsend_test.go` (c) and (d) | exit 1 observed where 6 expected (AC69) *(v2.1, V1)* |
| M76 | Set the in-flight flag after `Do` returns instead of before | `postsend_test.go` (a) | exit 1 observed (AC69) *(v2.1)* |
| M77 | Skip the credential comparison on `resume` | `resume_test.go` | request sent (AC70) |
| M78 | Map `REUSED` regardless of `reason` | `table_test.go` | `different_credential` → 3 (AC71) |
| M79 | Resume from `awaiting_confirmation` without asking | `resume_test.go` (pty) | execute count 1 where 0 expected (AC72) |
| M80 | Write `awaiting_confirmation` only on `y` | `resume_test.go` | `at_prompt` (after display, before read) leaves `not_started` where `awaiting_confirmation` is asserted (AC72) *(v2.1, V9)* |
| M81 | Resume a `pending` execute on a non-TTY without `--yes` | `resume_test.go` | request sent (AC73) |
| M82 | Apply the transaction rule to a simulate body | `table_test.go` | `completed` simulate → 0 where 4 expected (AC74) |
| M83 | Record `plan` from `result.body.plan` after a 202 | `broadcast_test.go` | execute step not `unreachable` / execute sent with `null` token (AC75) |
| M84 | Record without asserting `expect` | `recorded_interactions_spec.rb` "wrong scenario under a name" | green where red expected (AC76) |
| M85 | Loader ignores `expect` | `server_test.go` | serves a disagreeing directory (AC77) |
| M86 | Serialise the flag body through `map[string]any` with a random key | `body_test.go` | structural inequality with the recording (AC78) |
| M87 | Fault point `after_simulate_recorded_before_execute_begin`: resume re-simulates | `crash_test.go` | simulate count 2 (AC80) |
| M88 | Fault point `after_simulate_send_before_record`: resume executes with a `null` token | `crash_test.go` | execute count 1 where 0 expected (AC80) |
| M89 | Proceed when an orphan exists | `pending_test.go` | request count 1 where 0 expected (AC81) |
| M90 | Treat a locked run as an orphan | `pending_test.go` (two processes) | refusal where proceed expected (AC81, AC90) |
| M91 | `Record` stores the simulate body verbatim | `ledger_test.go` | `plan.token` present (AC82) |
| M92 | HTML `502` on money → 5 | `table_test.go` | 6 expected (AC83) |
| M93 | Add a file under `cli/` outside every prefix | `ownership_test.go` | (AC84) |
| M94 | Drop `subStatus` from the `Transaction` schema | `api_docs_spec.rb` | allowlist ≠ schema (AC85) |
| M95 | Add an undeclared field to the Go `Transaction` struct | `schema_pin_test.go` | (AC86) |
| M96 | Accept `--idempotency-key` with `--broadcast` | `userkey_test.go` | exit 0/… where 2 expected (AC87) |
| M97 | Delete a still-present defect from `DOC-DEFECTS.md` | `cli_docs_spec.rb` | (AC89) |
| M98 | Fixture `Hold` releases immediately | `server_test.go` | signal arrives after the response (AC91) |
| M99 | Retry `599` | `retry_test.go` | count 2 (AC27) |
| M100 | Unknown read 4xx → 1 | `table_test.go` | 3 expected (AC24) |
| M101 | SIGINT during `poll.Watch` → exit 1 | `postsend_test.go` (d) | 6 expected (AC69) |
| M102 | Drop `SIGHUP` from the declined signals | `resume_test.go` (pty) | state stays `awaiting_confirmation` where `declined` expected (AC72) |
| M103 | `runs decline` accepts a `pending` step | `runs_test.go` / `ledger_test.go` | `declined` written where `ErrStepMayHaveSent` expected (AC57, AC92) |
| M104 | `runs show` renders `body` verbatim | `runs_test.go` | token canary found in `body.plan_token` (AC57) |
| M105 | Probe holds the sidecar lock until the command exits | `ledger_test.go` (two processes) | concurrent resume gets `ErrRunLocked` where success expected (AC12, AC90) |
| M106 | Resume gives up on the first `EWOULDBLOCK` (no retry) | `ledger_test.go` (two processes) | `ErrRunLocked` during a probe's hold where success expected (AC12) |
| M107 | Prune unlinks the sidecar before the record | `ledger_test.go` | record survives with no sidecar; a subsequent holder recreates a new inode (AC92) |
| M108 | Orphan check before `FindByKey` | `userkey_test.go` | same-body resume refused where routed expected (AC94) |
| M109 | Ignore `exempt` in `Orphans` | `ledger_test.go` | matched run reported (AC90, AC94) |
| M110 | `transfers execute` skips the TTY prompt | `execute_test.go` (pty) | request sent where 0 expected (AC93) |
| M111 | `transfers execute` writes `awaiting_confirmation` only on `y` | `execute_test.go` (pty) | `at_prompt` leaves `not_started` (AC93) |
| M112 | `Simulation` schema lacks `meta` | `api_docs_spec.rb` / `schema_pin_test.go` | replay render has a key the schema lacks (AC85); Go `Meta` field unpinned (AC86) |
| M113 | Pin `Principal` from one render only | `api_docs_spec.rb` | `user` or `environment` nested shape missing (AC85) |
| M114 | Leave `List.data.items` as `type: object` | `api_docs_spec.rb` | seventh reference unresolved (AC85) |
| M115 | Store `token` in `plan_token` on a `--broadcast` run in state `declined` | `ledger_test.go` sweep | occurrence outside the two permitted fields/states (AC82) |
| M116 | AC63 e2e: emit `http: {}` on a local refusal | `refusals_test.go` | `http == null` expected (AC63) |
| M117 | AC62 e2e: skip the `commands` row count | `resume_test.go` | test mutation only; the property (one row per key) is what M3 breaks — declared a demonstration, not a control |

### 6.3 Declared non-controls

- **AC68** (the audit record itself). Its "mutation" would be deleting the
  record; the value is the observations in it.

Every other criterion has at least one row above; the count is 92 criteria
(AC1–AC68 from revision 1, twenty-one from revision 2, AC92–AC94 from
revision 2.1; AC79 and AC88 were allocated and withdrawn during drafting and
are not reused), 117 rows. M69 and M117 mutate a *test* rather than the
system and are demonstrations that the branch is non-vacuous, not controls;
each names the code-side row (M3) that is the control.

---

## 7. Adversarial analysis

### 7.1 The attack table

| # | Attack or accident | Answer | Cost at maximum |
| --- | --- | --- | --- |
| A1 | `kill -9` between `Begin` and `Do` | `pending`; `resume` sends the same key (with consent for execute) | One request. |
| A2 | `kill -9` between `Do` and `Record` | Same; the resend replays | One request. |
| A3 | Lid closed mid-poll | Exit 6 with the command id | Nothing. |
| A4 | `--idempotency-key` from a different transfer | Refused locally (AC54) | Zero requests. |
| A5 | Wrapper re-runs the command line on exit 6/7 | **Refused on iteration 2 by the orphan check (C18, AC81)** unless `--allow-pending` | Zero requests without the flag. |
| A6 | Human confirms after five minutes | `410 PLAN_EXPIRED`, exit 4, no re-simulate | Nothing. |
| A7 | Two terminals `resume` one run | Sidecar `flock` refuses the second (AC12); server dedups if both slip through | One extra request. |
| A8 | Credentials file `0644` | Refused (AC7) | Zero requests. |
| A9 | `FERRY_API_URL` pointed elsewhere with a stored token | Refused: bound to `api_url` (AC9) | Zero requests. |
| A10 | `--debug` pasted into a bug report | Redacted (AC28) | Nothing. |
| A11 | Plan token in `argv` | `--plan -`; single-use, ≤5 min | Bounded. |
| A12 | Fixture drifts from the API | Ruby `git diff`; `expect` on both sides; AC85/AC86 | Caught in the PR. |
| A13 | 429 storm | Same key within budget; exit 5 | Nothing moved. |
| A14 | `201` body the CLI cannot parse | Exit 6, not 0 | Nothing lost. |
| A15 | Live key under a profile a script thought was sandbox | `--env` refuses; `LIVE` prefix | Zero requests when asserted. |
| A16 | PAT for a money command | Refused locally; server `WRONG_TOKEN_CLASS` | Zero requests. |
| A17 | Ctrl-C while the execute request is on the wire | Exit 6, `pending`, run id printed (C17, AC69) | Nothing beyond the one request. |
| A18 | Ctrl-C at the prompt, then `runs resume` | `awaiting_confirmation`; resume asks or requires `--yes` (C16, AC72) | Zero requests without consent. |
| A19 | `keys create --login`, then `resume` of an older run | Exit 7 before any request (C19, AC70); if bypassed, `REUSED`+`different_credential` → 7 (AC71) | Zero requests. |
| A20 | Disk full during `Record` after a 201 | Exit 6 with the run id; the record is `pending` and the response is lost; resume replays | Nothing lost server-side. |
| A21 | Simulate answers 202 under `--broadcast` | Execute `unreachable`; exit 4 after polling; never `{"plan_token": null}` (AC75) | Zero execute requests. |
| A22 | Ctrl-C during a 120 s poll after a recorded 202, or a renderer crash after a recorded 201 | Flag is monotonic: exit 6, `answered` step, `runs show` (C17, AC69(c)/(d)) | Nothing beyond the one request. |
| A23 | Terminal closes (`SIGHUP`) at the prompt on a headless box; later runs blocked by C18 | `declined` written on `SIGHUP`; if `SIGKILL`, `runs decline <id>` clears it without a TTY (AC57, AC72) | Zero requests. |

### 7.2 What I could not close

**A wrapper that passes `--allow-pending` in a retry loop.** C18 makes the
default safe; the flag exists for genuinely concurrent callers and a wrapper
that adds it has opted out in writing. Documentation is the control for that
choice, and only for it.

**The plan token on disk for up to ten minutes past expiry.** Bounded (§5.3),
scrubbed in the safe direction, and never resumable without consent. Accepted.

**A recorder that records the wrong thing.** AC76 closes the named-scenario
half; a recorder that drives the right scenario but the recording is wrong in
a way `expect` does not name is still possible. The e2e job is the
compensating control for the happy path and five refusals.

**The fixture's par pricing.** A demo against the fixture shows `1.0000`
rates; `cli/README.md` says so.

**No server version handshake.** WD3.

**A `--yes` passed by a wrapper is consent.** C16 is about *where* consent
comes from, not whether an automated caller may give it. An agent that passes
`--yes` has consented; that is the design.

---

## 8. Assumptions and dependencies

### 8.1 Decisions settled in revision 2

- **D-1 — GitHub organisation `kurenn`**; the repository is
  `github.com/kurenn/ferry`.
- **D-2 — Module path `github.com/kurenn/ferry-cli`.** (A400 moved this out of the API repository; it was `github.com/kurenn/ferry/cli`.)
- **D-3 — Homebrew tap `kurenn/tap`**; install line `brew install
  kurenn/tap/ferry`. The landing pages' bare `brew install ferry` is being
  changed.
- **D-4 — No baked-in API URL.** There is no production host; a default that
  later resolves to a real one is a money risk. `auth login` requires `--api
  URL` or `FERRY_API_URL` and stores it on the profile; every later command
  uses the profile's URL and refuses an explicit one that differs (C7). The
  `openapi.yaml` `servers` placeholder is not used by the binary. *Reading
  taken:* "explicitly" means at login; the profile is the explicit record
  thereafter. If the intent is that every invocation must carry the URL, that
  is a one-line change to step 2 of §5.4 and AC9's message.

### 8.2 Assumptions

- **CS-1 — Go 1.24 via `actions/setup-go`** from `cli/go.mod`.
- **CS-2 — `cobra`, `pflag`, `yaml.v3` (tests), `x/term` are the whole
  dependency set.** *(v2: `x/sys` removed.)*
- **CS-3 — The Rails app serves HTTP under `RAILS_ENV=test` with
  `FERRY_OMS_BACKEND=fixture`** and the boot gates pass. *Check:* `GET /up` →
  200; `GET /v1/me` without a token → `401 TOKEN_MISSING`.
- **CS-4 — `Organizations::Provision` and `User.create!` work from `bin/rails
  runner`, and `ferry:pat:issue` prints the token on its own stdout line**
  (`ferry_pat.rake:158–170`).
- **CS-5 — A fresh `unconfigured` sandbox environment authenticates and the
  fixture serves it** (§1.2.17).
- **CS-6 — A sandbox policy row with NULL caps allows `usdc → usd`**
  (§1.2.18).
- **CS-7 — The recorder can reach every §5.10 scenario.** *(v2.)* Least
  certain: `execute.upstream_busy.with_command.503` (needs a proven never-sent
  pair; `socket_theatre` may be required); `execute.replay_expired.409` (uses
  `expire_replay_window!`, which sets **`replay_expires_at`**, not
  `state_changed_at` — CRITIQUE N1); `execute.key_reused.different_credential.409`
  (mint a second unlinked key with `credential_builders.rb`; replay under it).
  **Declared unreachable and not recorded:** `execute.destination_too_new.403`
  (§1.2.7), `UPSTREAM_CREDENTIALS_INVALID`, `UPSTREAM_AUTH_UNAVAILABLE`. *If a
  further scenario is unreachable:* the manifest records it with the reason,
  the Go row is asserted from the table alone, and §11.3 names the gap.
  **Specifically for `execute.upstream_busy.with_command.503` (V11):** the
  ledger-builder route does not reach it — a `failed_retriable` row replays
  as `202` (`replay.rb:71–72`), not as the live `503` — so the only recording
  path is the scripted gateway producing a never-sent pair at T2 so that
  `give_up` returns `Retriable` (`executor.rb:467–481`). If that cannot be
  driven, AC52's with-header half degrades to: the fixture serves the recorded
  `execute.upstream_busy.no_command.503` **body** with a `Ferry-Command-Id`
  header injected by the test and the manifest marking the scenario
  `synthesised_headers: ["Ferry-Command-Id"]`; the resend → recorded `202` →
  poll sequence is then asserted as written. C13 forbids hand-written
  *bodies*, not headers, and the CLI branches on the header (§5.6). The gap
  that remains — the body lacks `details.command_id`/`details.state` — is
  named in §11.3 and the `next` sentence for that row is asserted without the
  command id.
- **CS-8 — *(settled, D-1/D-2)*.**
- **CS-9 — *(settled, D-4)*.**
- **CS-10 — goreleaser can push a formula to `kurenn/homebrew-tap` with
  `HOMEBREW_TAP_GITHUB_TOKEN`.** Blocks the release workflow, not CI.
- **CS-11 — `syscall.Flock` with `LOCK_EX|LOCK_NB` on a sidecar file behaves
  as required on macOS and Linux.** U1 asserts with two processes after a
  rewrite (AC12).
- **CS-12 — `x/term` detects a TTY and reads without echo on both platforms,
  and the harness can allocate a pty** (AC48, AC72).
- **CS-13 — `poll` is always a relative path** (`command.rb:199`).
- **CS-14 — *(v2; v2.1, V8)* The serializers expose no key-list constant**
  (`api_key_serializer.rb`, `principal_serializer.rb` are literal hash
  builders), **so U8 pins against rendered objects**: two `Principal` renders
  (one API-key principal, one PAT) whose key sets are unioned so both nested
  shapes are present; one `ApiKey` render with `token:` and one without.
  U8 touches no serializer. *Blocks:* nothing; this is the design, not a
  fallback.
- **CS-15 — *(v2; v2.1)* SIGINT delivered to the `faultinject` binary while
  the fixture holds its request open is observable as exit 6.** *Fallback,
  one sentence:* the root's signal handler cancels the request context, `Do`
  returns through AC23's after-write branch, and the outcome is the same
  exit 6 — so AC69(a) holds by either mechanism. Not load-bearing for a start.

---

## 9. Deviations

### 9.1 From prior designs

- **WD1 — The client's structs are hand-written and pinned to the schemas,
  not generated.** *(v2, narrowed.)* The contract is fixed by U8 so that
  generation *would* work for types; the plan still hand-writes nine routes
  and pins field sets (AC86) so a later switch to `oapi-codegen -generate
  types` is a mechanical swap under the same test. §5.12.
- **WD2 — Credentials in a 0600 file, not the OS keychain.**
- **WD3 — No `Ferry-Client-Outdated` handshake.** No controller implements one.
- **WD4 — Exit codes differ from PLAN-V2 §14.2.4**: organised by what the
  caller does; 6 and 7 are the two a wrapper must never retry.
- **WD5 — `--output` is never inferred from the TTY.**
- **WD6 — Amendments are numbered from `A300`.**
- **WD7 — The Rails suite writes files under `cli/`** (the `db/structure.sql`
  pattern).
- **WD8 — `runs prune` is manual.**
- **WD9 — *(v2)* No compiled-in API URL** (§8 D-4), against PLAN-V2 §14.2.1's
  default host.

### 9.2 From the critique — findings altered or declined, with the evidence read

- **CRITIQUE M4 — accepted diagnosis, different remedy.** The critique's fix
  pins a Ruby serialiser to Go's output. Read: `TransfersController#simulate`
  passes `request.request_parameters` (`transfers_controller.rb:63`), a parsed
  hash — key order and whitespace are gone before the digest or translator
  sees them; the digest (`command_request.rb:126–128`) is over the raw bytes
  but is compared only against *the same client's* earlier bytes. A
  serialisation pin therefore buys nothing the server observes, and couples
  two languages' JSON emitters. v2 matches structurally (AC30), asserts byte
  identity Go-vs-Go at the request log (AC45, AC80), and asserts `--body`
  input-vs-wire (AC56). The residual — that Rails accepts Go's bytes — is
  e2e's, and is named in §5.10.
- **CRITIQUE B3 — accepted, stricter than the fix proposed.** The critique's
  `confirmed` state, once written, would let a later `resume` execute without
  asking. v2 has no `confirmed` state: consent is never stored (C16); `resume`
  asks or requires `--yes` for *every* execute send, including a `pending`
  one. Re-asking is free; a stored yes is the defect class B3 found.
- **CRITIQUE B1, unknown read code.** The critique offered 3 or 7; v2 uses 3
  for a 4xx and 5 for a 5xx (AC24), because a novel 5xx on a read is
  transient until proven otherwise and 7 would tell a human to escalate a
  `GET`.
- **CRITIQUE M9 — accepted with one refinement.** A `pending` record whose
  sidecar lock a live process holds is a concurrent run, not an orphan, and
  does not trigger the refusal (AC90). Without this, two legitimate parallel
  transfers would block each other and every concurrent caller would reach
  for `--allow-pending`, which is the opt-out the control depends on being
  rare.
- **CRITIQUE N2 — partially accepted.** Scrub-on-load is kept, with the grace
  widened to ten minutes and the local-clock nature stated (§5.3). It is a
  local decision in the safe direction only (a scrubbed token cannot send),
  which is the distinction §10.2's refusal rule draws.
- **CRITIQUE G2 — accepted; option chosen:** `--idempotency-key` with
  `--broadcast` is a usage error (AC87). Deriving `<K>-simulate` would invent
  a key the caller did not name.
- **CRITIQUE N3 — accepted.** The preamble now cites AC44/AC45/AC69/AC72/AC80.
  AC numbers are not renumbered.
- **CRITIQUE "The exit-code vocabulary, mapped" — `PLAN_KEY_MISMATCH`
  sentence corrected**: `Refused` rolls the consumption back
  (`begin.rb:47–51`); the row now says "rolled back, not spent".
- **No finding declined outright.** Each was checked against the file it
  cites; all held (§1.2.25–30).

### 9.3 From the verification of v2 (`V1`–`V11`)

All eleven were re-read against the files they cite before folding
(`command_request.rb:325–334` for `meta`; `openapi.yaml:641–657` for
`List.data.items`; `api_key_serializer.rb:40` for the conditional `token`;
`principal_serializer.rb:44–70` for the two null shapes;
`command_request_spec.rb:336–350` for `expire_replay_window!`). All held.
Alterations:

- **V1 — accepted as stated.** The flag is monotonic; abnormal exits after
  any money `Do` are 6; the normal path exits on its class. The critique's
  parenthetical alternative — "or the recorded class" for post-`Record`
  abnormal exits — is not taken: a process that crashed in its renderer
  cannot vouch for what it would have printed, and `runs show` reads the
  record.
- **V4 — accepted, both remedies.** `SIGHUP` joins the declined signals *and*
  `runs decline` exists, because `SIGKILL` and OOM leave no signal to catch.
  `prune` was not widened to `awaiting_confirmation`: prune is for records
  nobody needs to look at again, and an unconfirmed plan is a decision
  someone has not yet made. `decline` refuses `pending`, so it cannot
  convert unknown into no.
- **V5 — accepted.** Holder/probe/prune idioms are now the only three (§5.3).
  `fcntl` `F_GETLK` (a test-without-acquire) was considered and rejected:
  POSIX record locks are released when *any* descriptor on the file closes,
  which is a worse failure mode than a 250 ms retry.
- **V6 — accepted; claim restated rather than mechanism changed.** Scrubbing
  `body` and dropping `body_sha256` would remove the proof a resume resends
  the same bytes (AC11, AC45), which is a money property; the token in `body`
  is spent or expired by the time it matters, and display redacts it.
- **V7 — accepted; AC62 keeps a second observable.** `Idempotency-Replayed:
  true` alone proves the property; the `commands` row count via `bin/rails
  runner` is retained as the direct statement "one command exists for this
  key", since the e2e job already has the Rails app and database in hand.
- **V11 — accepted; the AC52 fallback is a header injection on a recorded
  body, and its residual gap is named** (§8.2 CS-7). The alternative — drop
  AC52's with-header half — would leave the one live transient failure the
  API documents with no behavioural test.
- **V3, V2, V8, V9, V10 — accepted as stated.**

---

## 10. Deliberately not built

### 10.1 With the condition that lifts each

| # | Not built | Lifted when |
| --- | --- | --- |
| 1 | `ferry wallets *`, `customers`, `external-accounts`, `transactions` | The API has the endpoints (§1.2.15). Landing copy must drop `ferry wallets list`. |
| 2 | `ferry auth login` that *obtains* a PAT | A PAT endpoint or device-code flow exists. |
| 3 | OS keychain storage | A laptop audience asks; `--store keychain` beside the file. |
| 4 | Local corridor cache; local pre-validation | Never for validation. Cache only if measured. |
| 5 | `brew install ferry` from Homebrew core | Core's notability bar is met. |
| 6 | Windows builds | `flock`, paths, pty harness ported. |
| 7 | Cosign signatures | Go-live artefact; checksums ship now. |
| 8 | Generated types | *(v2)* The schemas now exist (U8); generation is a swap under AC86 when routes exceed a dozen. |
| 9 | Background 202 follow-up | Never. |
| 10 | `transfers get <id>` | `GET /v1/transfers/{id}` exists. |
| 11 | MCP stdio shim | `POST /mcp` exists. |
| 12 | Dynamic-id completions | Never. |
| 13 | *(v2)* A stored `confirmed` state | Never (C16). |
| 14 | *(v2)* A default API URL | A production host exists **and** the risk in D-4 is re-argued. |

### 10.2 Explicitly refused, not deferred

- Deriving the idempotency key from the arguments.
- Auto re-simulating after `PLAN_EXPIRED`/`PLAN_CHANGED`/replay-without-token
  under `--broadcast` (C8).
- A `--force`/`--new-key` on `resume`.
- Inferring `--output` from the TTY.
- A local "is the plan expired?" *refusal* (the server's clock decides
  refusals; the local scrub is not a refusal, §5.3).
- Retrying `POST /v1/api_keys`.
- A documented `--no-precheck`.
- *(v2)* Exit 1 from any abnormal path after the in-flight flag is set (C17).
- *(v2)* Storing consent in any form (C16).
- *(v2)* Deriving a second key from a caller-supplied `--idempotency-key`.
- *(v2.1)* Clearing the in-flight flag for any reason (V1).
- *(v2.1)* `runs decline` on a step that has run `Begin` (V4): unknown is not
  declinable.
- *(v2.1)* Scrubbing the execute step's `body` or dropping `body_sha256`
  (V6): the resume proof outranks the residual secret.

---

## 11. Amendments and audit

### 11.1 How to amend

An implementing agent that finds this plan wrong changes the plan in the same
commit as the code, adds a row to §11.3, and — if an acceptance criterion
moved — says which and why. A narrowed assertion needs either the code fixed
or the claim fixed. Section numbers and AC numbers are never renumbered.

### 11.2 Mutation audit checklist

Filled in by U7 before AC68 is ticked. For each §6.2 row: the mutation
applied, file and line, the example that went red, restored from snapshot.
Rows touching `internal/fault/` or `e2e/` run with the build tag named; every
run is `go test -count=1`. Also recorded: CS-1–CS-15 measurements, e2e
wall-clock, binary sizes.

### 11.3 Amendment log

Reserved block `A300–A399` (WD6); per-unit decades: U0 `A300–A309`, U1
`A310–A319`, U2 `A320–A329`, U3 `A330–A339`, U4 `A340–A349`, U5 `A350–A359`,
U6 `A360–A369`, U7 `A370–A379`, U8 `A380–A389`, free `A390–A399`. Revision-2
plan amendments are `P1–P30` (planning, pre-implementation) so they do not
consume the implementation block.

| # | Section / AC | What changed | Finding | Mutation now covering it |
| --- | --- | --- | --- | --- |
| P1 | Preamble, §1 header | Base moved to `3239f06`; preamble cites AC44/AC45/AC69/AC72/AC80 | N3, G1 | — |
| P2 | §1.2.13 | Execute does not refuse unknown keys; digested and ignored | N4 | — (measurement) |
| P3 | §1.2.25–30 | Six new measurements: `REUSED` reasons, `REPLAY_EXPIRED.details.state`, `send_and_record` timing, serializers, `syscall.Flock`, flock-vs-rename | B1, B2, N1, N7, M7 | — |
| P4 | §1.4 | Rewritten as a status table against `6de569a`; items 1b, 7, 8 added | G1, M6, N4 | M72, M97 |
| P5 | C16 (new), §5.3, §5.4 step 8, §5.5, AC48, AC72, AC73, §10.1 row 13 | Consent is per invocation; `awaiting_confirmation`/`declined` states; `resume` asks or requires `--yes` for every execute send | B3 | M79, M80, M81 |
| P6 | C17 (new), §5.4 steps 10–11, §5.9, AC69, AC91, AC41 | In-flight flag; every post-send exit is 6; fixture `Hold`; fault points `record_fails_after_send`, `render_panics_after_201` | B1 | M73–M76, M98 |
| P7 | C4, AC20, AC24 | `details.reason` and `operation` are table inputs; unknown read code → 3/5 | B1, B2 | M78, M100 |
| P8 | C19 (new), AC70, AC71, §5.3 resume step 2, §5.6 `REUSED` rows | Credential identity on resume; `different_credential` → 7 | B2 | M77, M78 |
| P9 | C8, AC74, AC75, §5.6 terminal table, §5.10 `simulate.202.then_completed`, AC50 | Terminal rule by `operation`; simulate `completed` → 4; `--broadcast` 202 → execute `unreachable` | B4 | M82, M83 |
| P10 | §6.2 M63–M72 | Mutations for AC1–AC5, AC53, AC62, AC63, AC65, AC67; AC68 declared non-control (§6.3) | M1 | M63–M72 |
| P11 | AC62 | Asserted at both fault points with distinct expectations | M2 | M3, M69 |
| P12 | §4.1 U2a/U2b, U8 (new), §4.2 waves 1/1b/2, `harness.Run` signature, AC41 → U5, AC84 ownership lint | Waves re-drawn; `root.go` dependencies removed from U4 | M3 | M42, M93 |
| P13 | §5.10, AC3, AC30, AC78, C13 wording | Structural request matching; no `body_sha256` in recordings; placeholder token handed out; byte identity asserted Go-vs-Go | M4, N5 | M32, M86 |
| P14 | AC76, AC77, §5.10 format | Declared `expect` per scenario, asserted by recorder and loader | M5 | M84, M85 |
| P15 | §1.2.7, §5.10, CS-7, AC19 | `destination_too_new` declared unreachable; table row marked `unreachable` | M6 | M17 |
| P16 | C12, AC12, AC13, §5.3 | Sidecar `<run>.lock`; AC12 re-cut after a rewrite; scrub skips locked runs | M7 | M5 |
| P17 | AC80, §6.1 fault points | Two `--broadcast` inter-step fault points | M8 | M87, M88 |
| P18 | C18 (new), AC81, AC90, §5.1 `--allow-pending`, §5.4 step 3, §7.1 A5, §7.2 | Fail-closed on orphaned unresolved runs; lock-aware | M9 | M89, M90 |
| P19 | §5.6 `REPLAY_EXPIRED` row, CS-7 | `details.state` rendered; `expire_replay_window!` / `replay_expires_at` | N1 | — (render) |
| P20 | C6, AC13, §5.3 | Scrub grace 10 min; local-clock caveats stated | N2 | M6, M7 |
| P21 | C13, AC27, §5.6, §5.8 | Transport faults synthesised; `599` never retried | N5 | M99 |
| P22 | §6.2 M48 | Detector is "no prompt text and execute sent", not a timeout | N6 | M48 |
| P23 | §5.13, CS-2 | `x/sys` dropped; `syscall.Flock` | N7 | — |
| P24 | AC67, AC89, U7 files | Doc claims are a spec that re-measures, not a static edit | G1 | M72, M97 |
| P25 | AC87, §5.1, §5.3 | `--idempotency-key` + `--broadcast` is a usage error | G2 | M96 |
| P26 | AC82, C6, §5.3 | `Record` strips `plan.token` | G3 | M91 |
| P27 | AC83, §5.6 | Rows for non-envelope bodies and unlisted statuses | G4 | M92 |
| P28 | U8 (new), AC85, AC86, §5.12, WD1, §10.1 row 8 | Contract schemas added and pinned Rails-side; Go structs pinned to them | "type: object" | M94, M95 |
| P29 | §8.1 D-1–D-4, §5.2, §5.13, §5.15, AC9, AC35, WD9, §10.1 row 14 | Org, module path, tap settled; no baked-in API URL; `--api`/`FERRY_API_URL` required at login | user decisions | M12, M37 |
| P30 | §5.6 `PLAN_KEY_MISMATCH` | "Rolled back, not spent" | vocabulary note | — |
| P31 | C17, §5.4 steps 10–11, 14, §5.9, AC69 (c)(d), §7.1 A22, §10.2 | In-flight flag is monotonic; governs abnormal exits only; renderer panic and poll-time SIGINT after a recorded response are exit 6 | V1 | M75 (re-cut), M76 (re-cut), M101 |
| P32 | AC85, AC86, §5.12, U8 | `Simulation.meta.remediation` (replay-only) added to the pin; conditional properties are pointer fields | V2 | M112 |
| P33 | §5.4 steps 3–5, AC90 `exempt`, AC94, §5.3 orphan paragraph | `FindByKey` before the orphan check; the matched run is exempt | V3 | M108, M109 |
| P34 | C16, §5.3 state text, AC57, AC72, AC92, §5.1 `runs decline`, §7.1 A23 | `SIGHUP` declines; `runs decline <id>` for `awaiting_confirmation`/`not_started` only; prune leaves `awaiting_confirmation` | V4 | M102, M103 |
| P35 | C12, §5.3 lock idioms, AC12, AC90, AC92 | Holder retries ≤ 250 ms; probes release within the call; prune unlinks record then sidecar under the lock | V5 | M105, M106, M107 |
| P36 | C6, AC82, AC57, §5.3 "what the record holds" | Token's two resting places stated honestly; `body` not scrubbed; display redacts both | V6 | M104, M115 |
| P37 | AC62, AC63 | Observables named: `http.replayed`, `http.command_id`, `commands` row count; `http == null` | V7 | M116, M117 (demonstration) |
| P38 | AC85, U8, §5.12, CS-14 | Seven bodies incl. `List.data.items`; `ApiKey.token` conditional; `Principal` from two renders; no serializer edits | V8 | M113, M114 |
| P39 | AC72, §6.1 `at_prompt` definition, M80 | `at_prompt` = after display, before read; mutant = write only on `y`; AC72 asserts the state explicitly | V9 | M80 (re-cut) |
| P40 | AC93, §5.5, C16 | `transfers execute` on a TTY is C16's third arm with its own control | V10 | M110, M111 |
| P41 | CS-7, §4.1 U0 | AC52 fallback via recorded body + injected header, residual named; `expire_replay_window!` copied into U0's own support file | V11 | — |
| P42 | CS-15 | Context-cancel fallback named; not load-bearing | verifier | — |

| A300 | AC76, AC77, §5.10 format | `expect` gains `body_equals`, a path-to-value map; **AC77 belongs to U3, not U6**, and must decode it | AC76 demands `execute.key_reused.different_credential.409` expect `details.reason == "different_credential"`, but the `expect` vocabulary offered presence axes only. The *same* code and the *same* field answer the other reuse scenario with `different_request`, so presence alone would record either one under either name — the assertion AC76 names could not be written. | M65, and the AC76 refusals |
| A301 | AC76, AC77, §5.10 format | `expect` is an **array**, one entry per interaction | §5.10 drew it as a single object, which suits the 68 single-request scenarios and cannot express the four sequences: a 202 then two polls is three answers, and one object collapses to whatever answered last. Uniform arrays let the Go loader decode one type. | M84 |
| A302 | §5.10 `execute.internal_error.500` | Recorded by swapping `Commands::Executor.call` for one returning an unrenderable result, restored in `ensure` — **not** rspec-mocks | §5.10 listed the scenario as recordable but gave no HTTP route to a 500 on `/v1/transfers` outside the `else` arm of `render_command_result`. An `allow` leaks across scenarios, because every money scenario is recorded inside one example — it did leak the 500 into the next scenario's name on the first attempt. | U0's restoration control |
| A303 | §5.10 format example | Idempotency placeholder is `IDEMPOTENCY_KEY_PLACEHOLDER_1`, indexed like every other kind, not `KEY_PLACEHOLDER` | The illustration used an unindexed name the recorder does not emit; a unit that copied the illustration would match nothing. | pinned by AC3 |
| A304 | §5.6 decision table, §1.2 | `EXECUTION_SUSPENDED` and `PLAN_KEY_MISMATCH` on execute are decided in **T1, after** the T0d quote re-read | A scenario that scripts no `get_quote` records `UPSTREAM_BUSY` under their names — the recording would be filed under a code it does not contain. Affects anyone adding execute scenarios. | U0's scripted-quote scenarios |
| A305 | AC76 | The AC76 probe scenarios record into a scratch directory, asserted empty after each refusal | With the assertion removed the probes stopped raising and therefore **wrote** — into `cli/testdata/recorded/`, under names no Go test knows, and one file was still there after the run. The control proving refusal was itself contaminating the fixture set. | M84 |
| A306 | §5.14, §4.1 U6 | The `test` job's `git diff --exit-code cli/testdata/recorded` step lands with U0 | Same ordering defect as A315: assigned to U6, it would have sat unwritten for four waves while the recordings it checks were already committed, so byte-stability was proven only on the machine that recorded them. Reproducibility on a second machine is what AC2 and AC4 claim and what the fixture server rests on. | the step is itself the control |
| A320 | §4.1 U2 split | U2b **wrote** `cli/internal/api/responses.go`; it did not narrow existing structs | §4.1 said U2b owned "the typed body structs' final field sets", implying U2a had written them. U2a wrote only the request bodies and said so at `client.go:103` — the five response structs did not exist. The file sits inside a prefix `cli/OWNERSHIP` already gives U2, so AC84 held and no prefix was added. | M86 and AC86's pin |
| A321 | AC85, §1.4 item 3, `openapi.yaml` | **Four** object nodes remain untyped, not one: `Error.error.details`, `Command.result.body`, `Command.last_error`, `Command.contradiction` | §1.4 item 3 records the "everything is `type: object`" class as closed by U8. It is closed for seven bodies, not for these four — and every one is load-bearing for a money decision: `details.reason` drives C19's safety-critical reuse split, `details.command_id` is how a 409 is polled, `details.state` is what `REPLAY_EXPIRED` renders, `result.body.status` is exit 0 versus exit 8 on a transfer that may have moved money, and a non-null `contradiction` overrides `failed_terminal`'s "no money moved" reading. U2b found `result.body`; a document-wide survey found the other three. | assigned out; pins to follow |
| A330 | AC91 | `Hold(scenario)` returns an **arrival channel** as well as a release function | A release function alone cannot be used without a race: the caller has no way to know the request is on the wire before releasing it. | M98 |
| A331 | AC30 | Credential class is a match axis | The recordings carry it and FERRY decides on it first, so ignoring it would answer a PAT on `POST /v1/transfers` with the 201 reserved for an API key — the fixture would be more permissive than the API, teaching every downstream unit a false lesson. | U3-11 |
| A332 | §6.2 M41, AC40 | **M41 cannot be detected at the HTTP layer; U4 must assert it on what `api.Expand` returns** | `net/url` escapes a raw `>` when writing the request line, so `walletCrypto->bankUs` and the `%3E` spelling put **identical bytes** on the wire — measured and independently reproduced. The fixture sees `%3E` either way and answers both, so the mutation named in §6.2 is unkillable as specified. `TestThePathIsMatchedAsTheEscapedRequestTarget` pins the measurement. | M41, re-anchored |
| A333 | AC77 | An expectation asserting nothing beyond its status is refused | An entry reduced to `{status}` passes against almost any answer, so the axis vocabulary would be decoration. | U3-10 |
| A334 | AC30 | The fixture compares **query values**, not the raw target | The recorder writes `?limit=2&cursor=…` and `url.Values.Encode` sorts to `cursor=…&limit=2`. Nothing semantic differs, but a matcher comparing raw targets would have failed U4's first list test — CRITIQUE M4's failure mode arriving through the query rather than the body. | U3-14a, U3-14b |
| A335 | §5.10, U0 files | **Open gap, not fixed:** `spec/support/cli/recorder.rb:553` accepts `content_type:` but `:562–571` does not capture it, so media type cannot be a fixture match axis | `auth.unsupported_media_type.415` rests entirely on its verbatim `name=a` body. A unit sending JSON under a form content type would be answered the recorded 201 where FERRY answers 415 — the fixture is more permissive than the API on exactly the axis the scenario exists to cover. Needs a re-record, so it is U7's to judge rather than a drive-by. | — (none yet; that is the gap) |
| A390 | §5.10 builder-route paragraph | The enumeration of rows taking the build-and-replay route names four; `execute.contract_violation.502` also takes it | Enumeration short by one. The route itself is described correctly and the manifest records provenance faithfully. | — (prose) |
| A391 | §5.10, AC77 | A constructed recording's `Idempotency-Replayed` and `Ferry-Command-Id` are **not** evidence about the live path | `execute.contract_violation.502` carries both, which reads as "the plan was consumed" while §1.2.5 says a pre-send refusal does consume it — the two nearly contradicted. Both are in fact true: that row was seeded, so its headers describe the seeded row. Reading a constructed recording's headers as live evidence is now refused, with a positive control proving the guard fires. | M-R13 |
| A392 | §5.6, `Command.last_error` | `last_error.code` is **not** the wire error vocabulary; a `failed_terminal` command may not be classified `transient` or `pending` whatever code it carries | The recordings carry `UPSTREAM_UNKNOWN` in `last_error.code`, which `errors.md` does not name at all — the overlap with the catalogue is coincidental. Reading that field through the wire table let `RATE_LIMITED`, `SERVICE_UNAVAILABLE` and `UPSTREAM_BUSY` classify `transient` ("resend the identical request", which also asserts `money: no`) and `INTERNAL_ERROR` classify `pending` ("keep polling") — both sending a caller back to a command `openapi.yaml` declares will never move. The unknown-code branch already refused this; applying the rule only there left the *known* codes able to say "come back" about something finished. Latent, not witnessed: every current `Ledger::Fail` caller passes a code that classifies `refused_*`. **Anyone typing `Command.last_error` must not declare its `code` as the `errors.md` enum.** | M-R11, M-R12, and the 59-row sweep |
| A393 | §1.4 item 3, §5.12, AC85, AC86, `openapi.yaml` | The four bare object nodes A321 named are typed and pinned both ways: `ErrorDetails` (open, 37 keys swept from the Ruby emitters), `Command.result.body` as a `oneOf` of `Transaction` and a new `StoredSimulation` discriminated on `object`, `last_error` (open, 15 keys), `contradiction` (closed, 6). The Go reader resolves `oneOf` into arms; `allOf`, `anyOf`, `not` stay fatal. | §1.4 item 3 claimed the class closed by U8. U8 closed the seven *response bodies*; these four live **inside** them. `ErrorDetails` is open by judgement, not omission: four codes emit more than one shape under one code, so no closed set is true of every answer, and no code-to-details mapping exists in the code to derive a union from — the pin, not the schema, is what holds it honest. `StoredSimulation` is `Simulation` minus `plan.token` and `meta`, both structurally absent, so **polling a completed simulate never yields a usable plan token** — asserted against the live render. | MF, MG, R7, and MG's deliberate survival |
| A394 | AC86, `Command` | **Open:** `Command` has no Go struct, so `state`, `last_error` and `contradiction` have no Go pin | The poll loop that would decode them is U5's and has not landed. The Rails side is pinned; the Go side cannot be until the struct exists. | — (that is the gap) |
| A395 | `api_docs_spec.rb` | **Open:** `responseSchemaNames` stops at a schema boundary, so a component referenced only from inside another is never surfaced | `StoredSimulation`, `Quote` and `ErrorDetails` are all reached that way and are pinned by hand. The collector was extended to follow union arms, which closes the case of a union answered directly — not this one. | — |
| A396 | `errors.rb`, `errors.md`, and A392 | **Open:** `UPSTREAM_UNKNOWN` and `RECOVERY_SWEEP_FAILED` are written into `last_error.code` and appear in neither the catalogue nor `errors.md` | Independently corroborates A392 from the other direction: `last_error.code` is not the wire vocabulary. Both are documented on the schema; giving them catalogue rows is a change to `errors.rb`. | — |
| A397 | `Corridors::QuoteRequest#side_body` | **Open:** it writes an `amount` key into the OMS instrument's own `details`, colliding by name with the error envelope's `details` | Excluded from the sweep by name, with that reason recorded in the exclusion map. | the exclusion map's own control |
| A398 | §5.6, `errors.rb`, `errors.md`, `api_docs_spec.rb`, new `Commands::LastError` | **Closes A396 by refusing its premise.** `UPSTREAM_UNKNOWN` and `RECOVERY_SWEEP_FAILED` are **not** missing catalogue rows and must not get any. `last_error.code` and the wire catalogue are two vocabularies whose members coincide *conditionally on state*, which is stronger than A392's "coincidental": `render_stored_error` is the only reader that resolves a `last_error.code` through `CATALOG`, it runs only on a `:stored` replay, and `Ledger::Replay::KINDS` grants `:stored` to the two terminal states alone. The five writers that park a command write onto `Lease::TAKEABLE` states, which answer `:command_body`. The one edge from a parked state into a terminal one goes through `Ledger::Fail` — the sole `to: "failed_terminal"` in the tree — whose `error_document` builds a **fresh** hash paired with an HTTP `status:` rather than merging, so a parked code cannot survive into a replayable row. The two codes are therefore given a declared vocabulary of their own, `Commands::LastError::LEDGER_ONLY`, rendered into `errors.md` as a third census ("Codes that are not errors", beside orphans and unreachables) and pinned to the emitters by set equality in both directions. Cataloguing them would have required inventing a status for a code that is never a status, made `Errors.fetch` stop raising for them, and — the real cost — turned `render_stored_error`'s guard from a loud refusal into a rendered HTTP error at a fabricated status, which is A392's CLI defect rebuilt on the server. | A396 read "written into a field the API renders, absent from the catalogue" as a documentation hole. It is a namespace boundary. The hand-typed `%w[UPSTREAM_UNKNOWN RECOVERY_SWEEP_FAILED]` in `api_docs_spec.rb` and the same pair in `NON_CODE_IDENTIFIERS` both now read `LEDGER_ONLY`, so a third such code needs no edit in either place to be tolerated — and cannot be added without a census that names it. | M1–M10; the one-directional survival at M4 |
| A399 | A396's writer count; `errors_spec.rb`'s wire sweep | **Open, two readings to correct.** (1) A396 records that three of `last_error`'s seven writers merge onto the row; it is **four** — `Recovery::Run#hand_back_quote`, `#escalation_error`, `#hand_back_error` and `RecoverySweep#isolation_error` merge, while `Ledger::Fail`, `RecordUnknown` and `RecordNeverSent` write fresh. The direction of the error matters: `Fail` being a fresh writer is the load-bearing half of A398, and counting it among the mergers would make the argument false. (2) `errors_spec.rb`'s wire-vocabulary sweep structurally **cannot see** `UPSTREAM_UNKNOWN`: its four conventions read code-shaped strings in wire *positions* (`Errors.fetch("X")`, a `code:` keyword, a `code`/`status` table, a `*_CODE` constant), and `"code" => "UPSTREAM_UNKNOWN"` is a string-keyed hash assoc, which is none of them. That is why `RECOVERY_SWEEP_FAILED` needed a `NOT_WIRE_CODES` exclusion and `UPSTREAM_UNKNOWN` never did, and why A396 had to arrive from the other direction. A398's census closes this for `last_error` specifically; a non-wire code written as a plain hash value into any *other* document is still invisible to both sweeps. | Neither is a live defect. (2) is a bound on a control, and recording it is what stops the next reader treating that sweep's silence as coverage. | — (that is the gap) |
| A340 | §5.10, AC37, U0 files | **Open gap:** `keys create --login` cannot be driven end to end, because the recorder substitutes `API_KEY_TOKEN_PLACEHOLDER_1`, which matches neither token regex, so `creds.Put` refuses it by shape | The store path is covered by an internal test with a well-shaped token and the reachable path end to end, but the two are not joined — on a command that persists a credential. Proposal: the recorder emits a **well-shaped canary** for `token` only. Needs a re-record, so it pairs with A335. | partial; the join is the gap |
| A341 | `internal/api` | **Open:** `internal/api` declares no list envelope, so `keys` and `corridors` each declare one locally | Two declarations of one wire shape, neither pinned to `List` in `openapi.yaml`. U2's files, so U4 did not add it. | — |
| A342 | AC40 | AC40's test lives in `internal/noun/corridors/corridors_test.go`, not the `get_test.go` the AC names | Cosmetic, recorded so the AC and the file agree. | M41 |
| A343 | §6.2 M39 | **M39 as written is a no-op**: `expires_at` carries `,omitempty`, so assigning `""` is exactly what correct code already does | Reported `APPLY-FAILED`, not "survived" — the distinction the protocol requires. Two working variants replace it. | M39a, M39b |
| A344 | AC33 | AC33's echo-off half **cannot** be asserted behaviourally: `harness.Run` puts its pty in raw mode before the command runs, so the transcript cannot witness echo state | Carried instead by a function-pointer identity check against `term.ReadPassword` plus a seam test. Recorded so a later reader does not mistake the absence of a pty assertion for an omission. | the identity check |
| A345 | §6.2 M41, A332 | **A332 confirmed empirically, not just by argument**: the same mutation was run against both layers — killed at `api.Expand`, and **survived green at the HTTP layer while the CLI had stopped encoding entirely** | The vacuous control A332 predicted, now measured. Worth keeping because it is the clearest demonstration in the slice that the layer an assertion sits at decides whether it is a control at all. | M41 at `Expand`; M41-HTTP recorded as a deliberate survivor |
| A310 | §5.3 record schema | `body` is a JSON string holding exact bytes, not an embedded object; non-UTF-8 refused at `Begin` | A `json.RawMessage` is compacted and HTML-escaped on re-marshal, so a rewritten record stops matching its own `body_sha256`. AC11, AC45, AC56 were unsatisfiable as drawn. | M11 |
| A311 | §5.3 record schema, AC83 | Response bodies split: JSON under `response.body`, anything else under `response.body_text` | `json.RawMessage` fails to marshal any non-JSON byte string, so `Record` wrote **no record at all** for an HTML `502`, a truncated body or an empty one. AC83 names HTML `502` as `pending`/6 — the class meaning the money may have moved — so the outcome a caller most needs on disk was the one that could not be written. | M12, A5 |
| A312 | C16, AC92, §5.3 state text | `runs.AllStates()` is the single census, held complete against the package's own AST; the five state tables assert the case reaches the state and their union against the census | `TestNoStateMeansTheHumanAgreed` iterated a hand-written list while claiming to cover the enum, so a later `StateConfirmed` would never have entered it — a one-directional subset check (`docs/dev-loop-learnings.md`) on the control protecting consent. Fixing it exposed two real gaps: `Decline` on `unreachable` is a no-op that does *not* end `declined`, and at `answered` the plan token rests in **both** `plan_token` and `body`, V6's two-place claim at the one state nothing checked. | C1, C1b, C2, C3 |
| A313 | §5.13 | `x/sys` is indirect via `x/term`, unavoidable; `x/term` pinned `v0.40.0`; `pflag` indirect; no unit after U1 runs `go mod tidy` | §5.13 said "**Not** `golang.org/x/sys`" as though the module graph could exclude it. No CLI code imports it, but `x/term` requires it. `v0.41.0+` needs Go ≥ 1.26 against a `go 1.24.0` declaration. | M93 (ownership lint) |
| A314 | §5.11 `harness.Run` | `harness.Run` is not parallel-safe — no caller uses `t.Parallel()`; with `tty=true` stdout and stderr are one merged transcript returned in both values | It swaps process streams and uses `t.Setenv`. A pty has one buffer, so a non-empty `stderr` must not be read as the command having written there. The pty is Linux-only by raw `ioctl` rather than adding a dependency outside §5.13; `pty_other.go` fails loudly, because a consent test that silently ran headless would assert AC49 while claiming AC48. | — (constraint on callers) |

| A315 | §5.14, §4.1 U6 | The `cli` CI job lands with U1 in minimal form (`setup-go`, `gofmt`, `vet`, `test -race`, `go.mod` freeze check); U6 keeps only the goreleaser and `faultinject` steps | §5.14 gave the whole job to U6, several waves after the units writing the code it checks, so U1–U5 would each have merged Go verified by nothing but a coordinator running `go test` by hand. Ordering defect, not a disagreement about content. | the job is itself the control |

Implementation amendments (`A300+`) begin when implementation does.

| A400 | §4.1, §8 D-2, §5.14 | The CLI is its own repository, `kurenn/ferry-cli`, module `github.com/kurenn/ferry-cli`. `docs/api/openapi.yaml` and `docs/api/errors.md` are vendored under `contract/`, digested in `contract/SOURCE` and held to those digests by `contract_test.go`. This plan and its critique moved to `docs/`, and are no longer in the API repository at all — one copy, here. `OWNERSHIP` gives `contract/` and `docs/` to U1, `.github/` to U6. | A product decision, not a technical finding: the CLI and the API ship to different people on different clocks. The cost is that the AC85/AC86 pins and the `internal/outcome` catalogue checks now read a copy rather than the generated file. Vendoring keeps them running; the digests make a local edit loud. | the digest check, shown failing on a one-line edit to `contract/errors.md` |

| A401 | A400's vendoring gap | **Open, and already observed once.** Within the hour of A400 landing, `#32` changed `docs/api/errors.md` in the API repository and this repository's vendored copy went stale — the exact failure A400 named and declined to fix. It is worse than a stale file: `internal/outcome/catalogue_test.go` reads `contract/errors.md` to check the decision table against the catalogue, so a code added upstream is a code this repository's own checks agree does not exist. The digest check cannot see it, because the copy still matches the digest recorded *for the copy*; `contract/SOURCE` names the upstream commit, but nothing compares it to upstream's HEAD. Closing it needs a cross-repository read — a scheduled job here, or a step in the API repository's CI — and either needs a token neither repository has today. | Recorded rather than papered over, because the first re-vendor was manual and the second will be too, and the failure mode is silence rather than a red build. | — (that is the gap) |
