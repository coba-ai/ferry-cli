# The mutation audit

AC68. §6.2 of `PLAN.md` allocates 117 named mutations across 92 acceptance
criteria. This is the record of what was actually observed, reconciled from
the eight unit pull requests plus the follow-up fixes, re-measured by U7 where
the record was silent and the criterion was a money one, and closed out by a
later pass that ran every remaining silent row. 116 of the 117 now carry a
verdict; the one that does not is M97, which cannot be measured from this
repository at all (A373).

§6.3 declares this document a **non-control**: its "mutation" would be
deleting it, and the value is the observations rather than a test that reads
it. Nothing here is asserted by a test, and that is deliberate. What *is*
asserted is `internal/adversarial_test.go`, which holds §7.1 to its tests, and
`spec/docs/cli_docs_spec.rb` in the API repository, which holds §1.4 to its
documents.

## The headline

Two findings, and they point opposite ways. **One silent row was hiding a
real money-path gap** (M77, below), which is why the rest had to be run.
**The rest were not**: all sixteen remaining measurable silent rows were run
in the closure pass and fifteen died, including all seven of the money-path
ones. The sixteenth survived and is an equivalent mutant. No code needed to
change. Neither half could have been asserted without measuring it.

### M77 — a money-path control was missing and is now written

§6.2 M77 ("skip the
credential comparison on `resume`", AC70, §7.1 A19) was never reported by any
unit. Applied against `main` it **survived**, twice:

| Mutation to `internal/noun/transfers/money.go` | Against `main` | After this PR |
| --- | --- | --- |
| `checkCredential`'s **token-prefix** arm made unreachable (`case false && …`) | **SURVIVED** | KILLED by `TestResumeRefusesACredentialWhoseTokenPrefixChanged` |
| `checkCredential`'s principal arm answers `ClassCLIFault` instead of `ClassEscalate` | **SURVIVED** | KILLED by `TestResumeRefusesAChangedCredential` |

Both are `go test -count=1 -tags faultinject ./...` over the whole module.

The code is correct; the *tests* were not. Two distinct gaps:

1. **The token-prefix arm had no example at all.** `checkCredential` compares
   the stored token's prefix and then the principal id. AC70 says "`token_prefix`
   **or** `principal_id`", and the only test drove the second. The arms are not
   redundant: the principal comparison is guarded by
   `match.Credential.PrincipalID != ""`, so for any profile stored without a
   principal id — which `creds.Put` will write — the prefix comparison is the
   only guard between one principal's pending transfer and another's key.
2. **The exit code was not asserted.** The test said `if exit == 0`, so exit 1
   passed. The difference is not cosmetic: `escalate`/7 answers *money: unknown,
   same key safe: no*; `cli_fault`/1 answers *money: no, same key safe: yes*.
   The run being resumed is `pending`, so "money did not move" is exactly the
   claim nobody can make about it, and a wrapper reading exit 1 would mint a
   fresh key over a transfer that may have executed.

Recorded as A370. This is the finding the audit was for: an unreported row is
a mutation nobody ran, and one of them was hiding a real gap.

## §6.2 reconciliation

| Disposition | Rows |
| --- | --- |
| Reported by the owning unit, as the plan wrote it | 81 |
| Reported under the planned number, different edit applied | 10 |
| Reported by a follow-up PR under local numbering, with a plan cross-reference | 3 |
| Re-measured by U7 in the first pass of this audit | 5 |
| **Measured by the audit closure** (the 16 measurable silent rows, plus M70) | **17** |
| **No verdict on record anywhere** | **1** |

The one remaining is M97, and it is not a miss: U7 established it cannot be
killed from this repository (A373). Every other silent row now has a verdict
and a named killer, below.

An unreported mutation is not a mutation that passed; it is a mutation nobody
ran, and M77 is the proof that the distinction has teeth. The closure below is
the other half of that proof, and it points the other way: sixteen rows were
run and fifteen died. Both halves had to be measured to know which was true.

### Reported under the planned number with a different edit (10)

These are substitutions, not misses. Each unit applied something the planned
edit could not reach, and said so. They are listed because a reader comparing
§6.2 to a PR table will otherwise think the row was mis-cited.

| # | §6.2 wrote | The unit applied | Unit |
| --- | --- | --- | --- |
| M27 | `allowed_assets` as `[]string` | coerce a null list to `[]` | U2a |
| M31 | `200 {}` for unrecorded | unrecorded request answers `200 {}` | U3 |
| M36 | Ignore `--env` on login | drop the `assertEnvironment` call | U4 |
| M43 | Send when the class is missing | `creds.RequireEither` regardless of the operation | U4 |
| M51 | Keep polling on `needs_operator` | never stop on a non-`pending` class | U5 |
| M75 | Clear the in-flight flag when `Record` returns nil | M75a add a `Clear` method; M75b panic after a send | U5 |
| M79 | Resume from `awaiting_confirmation` without asking | `--yes` is always granted | U5 |
| M83 | Record `plan` from `result.body.plan` after a 202 | M83b never mark execute unreachable | U5 |
| M101 | SIGINT during `poll.Watch` → exit 1 | M101a drop the pre-`Begin` interrupt check | U5 |
| M110 | `transfers execute` skips the TTY prompt | skip the consent call | U5 |

U5 additionally reported **M76, M101 and M83 as written are no-ops** — the
branch each names is redundant with another, so the mutant is equivalent and
its survival means nothing. It replaced each and said which. That is the
protocol working, and it is worth more than the ten substitutions above.

### Run by a follow-up PR, not by a unit (3)

`coba-ai/ferry` #18 (AC85, "type the five response bodies") numbered its
mutations locally from M1 and cross-referenced three §6.2 rows explicitly. A
reconciliation keyed on the number alone reports these as unreported, and one
keyed on the number alone across *all* PRs puts that PR's local M1 on top of
§6.2's "send before `runs.Begin`". Both readings are wrong; this is the right
one.

| §6.2 | Reported as | Verdict |
| --- | --- | --- |
| M94 (drop `subStatus` from `Transaction`) | #18 M13, "(PLAN M94)" | KILLED |
| M113 (pin `Principal` from one render only) | #18 M20 and M21, "(PLAN M113)" | KILLED, both renders |
| M114 (leave `List.data.items` as `type: object`) | #18 M11 | KILLED |

### Re-measured by U7 (5)

| # | Mutation | File | Verdict | Killed by |
| --- | --- | --- | --- | --- |
| M3 | Mint a fresh key on `resume` | `noun/transfers/money.go:489` | **APPLY-FAILED** — `ulid.New()` needs an import this file does not have; a build failure is not a kill | — |
| M3b | Resume resends under `step.IdempotencyKey + "-resumed"` | same | KILLED | `TestResumeAfterTheSimulateWasRecordedExecutesTheStoredPlan` + 4 others |
| M62 | Delete a §7.1 row | `docs/PLAN.md:1849` | KILLED | the whole census (via the `minRows` floor) |
| M72 | Restore "no CLI … neither exists" to `AGENTS.md` | `docs/api/AGENTS.md:400` | KILLED | `cli_docs_spec.rb`, two examples |
| M77 | Skip the credential comparison on `resume` | `noun/transfers/money.go:398,408` | **SURVIVED against `main`** — see the headline | now KILLED |
| M81 | Resume a `pending` execute on a non-TTY without `--yes` | `consent/consent.go:119` | KILLED | `TestADeclinedTransferIsNotSentByAResume` + 3 others |

M3's failure is worth keeping rather than quietly replacing: the plan's text
is a mutation that does not compile, so any unit that had tried it would have
hit the same wall.

### Named in prose only — M70, since tabulated

**M70** (make `--no-precheck` the default, AC63). U6 narrated it in the PR
body — it survived when only `OpSimulateTransfer` was flipped, because the
money path independently acquires an api-key-only `get_command` session, and
died when all three rows were flipped. There was no table row, so a reader
counting rows found nothing: the verdict existed and the record of it did not.
Both forms have now been re-measured and both reproduce; the rows are under
"M70, which had a verdict and no row" below.

### Why the seventeen were silent

Eight of the seventeen were U6's, and the reason is structural rather than
careless: **U6's PR body reports a count and not a table.** "25 applied. 21
KILLED, 2 SURVIVED, 2 superseded by a corrected application" — with the five
false KILLEDs it caught described at length, which is the most valuable
paragraph in any of the eight PRs. But no row was attributable to a §6.2
number, so eight rows covering CI, the e2e suite and the release could not be
reconciled. Recorded as A371, and **A371's finding stands whatever the numbers
turned out to be**: all seven of U6's measurable rows reproduce below, and the
verdicts were still unrecoverable from the repository. A report that cannot be
reconciled is a process defect independently of whether it was right.

Seven of the remaining nine were U5's, five of those money-path. U5 applied 30
mutations and reported every one; these were simply rows it did not reach.
Recorded as A372. U7 did not run them and said so rather than running five and
implying the rest.

**M97 is U7's own and is only half-measurable**, and is the one row still
without a verdict. It says "delete a still-present defect from
`DOC-DEFECTS.md`" and expects `cli_docs_spec.rb` to go red. `DOC-DEFECTS.md`
is in this repository and the spec is in the other one, and the spec
deliberately does not read across that boundary — reaching across the
filesystem is the defect A400 existed to remove. The half that is measurable —
delete a claim from the spec's own §1.4 set — was applied and killed. The half
that is not is A373, which stays open.

## The audit closure: the seventeen, measured

Sixteen of the seventeen, plus M70. One mutation at a time; snapshot, apply,
assert the edit landed on the intended line **with an occurrence count**,
run, record, restore from the snapshot and `cmp` byte-identical before the
next. Every money-path row was run over the whole module in both build
configurations (`go test -count=1 ./...` and `-tags faultinject ./...`)
rather than over its own package, because a row killed only from a distant
package is worth knowing about — and two of them are exactly that.

**The headline, and it is the opposite of M77's.** All seven of U5's
money-path rows were already defended. None was a coverage gap, so there is
no survived-then-died pair in this PR and no code changed. That is a result
rather than an absence of one: M77 established that a silent row can hide a
real gap, and the only way to know whether these seven did was to run them.
A426.

### Priority 1 — U5's nine (AC41, AC46, AC47, AC49, AC78, AC80, AC93)

| # | Mutation applied | File | Verdict | Killed by |
| --- | --- | --- | --- | --- |
| M45 | `Record` skips `stripPlanToken` when the run has one step | `runs/ledger.go:420` | KILLED | `TestCreateWithoutBroadcastSimulatesOnly` |
| M46 | A `refused_resimulate` execute is followed by a second simulate | `transfers/create.go:199` | KILLED | `TestBroadcastDoesNotResimulateOnARefusal` |
| M47 | The execute key is re-minted from a fresh ULID after the simulate 201 | `transfers/create.go:180` | KILLED | `TestBroadcastPreMintsBothKeysBeforeSending`; also `TestResumeAfterTheSimulateWasRecordedExecutesTheStoredPlan` |
| M86 | The flag body is re-serialised through `map[string]any` with a random key | `transfers/body.go:170` | KILLED | `TestTheFlagsProduceTheRecordedBody` (AC78's own) and 18 others |
| M87 | A resume of the inter-step window re-sends the simulate before executing | `transfers/resume.go:136` | KILLED | `TestResumeAfterTheSimulateWasRecordedExecutesTheStoredPlan` — **only under `-tags faultinject`** |
| M88 | `broadcast` proceeds to the execute when the simulate handed over no token | `transfers/create.go:167` | KILLED | `TestResumeAfterAnUnrecordedSimulateCannotExecute` (AC80's own); also `TestBroadcastReplayedSimulateNeverExecutes` |
| M111 | A one-step run writes `awaiting_confirmation` after `y`, not before the prompt | `transfers/money.go:781` | KILLED | `TestResumingAnUnsentStepWithNoTerminalIsOne` (the AC93 `at_prompt` control) and 3 others |
| M42 | The JSON usage document goes to stderr instead of stdout | `cli/root.go:362` | KILLED | `TestJSONModeWritesExactlyOneDocument/a_usage_error` (AC41's own) and 2 others |
| M49 | The execute render prints `✓ success` instead of "accepted upstream … not settled" | `transfers/report.go:151` | KILLED | `TestExecuteRenderDoesNotClaimSuccess` |

**M87 is the row to read.** It survives `go test -count=1 -race ./...`
**entirely** — zero failures across all 26 packages — and dies only under
`-tags faultinject`. That is correct rather than wrong: AC80's window is
reachable only through a fault point, and a fault point is a no-op without
the tag (`internal/fault/inject_off.go`). But it means the control for "a
resume never re-simulates" is invisible to anyone who runs the default suite,
and a reader who dropped the tagged run from CI would take a money-path
control with it and see nothing go red. CI does run both. A427.

### Priority 2 — U6's seven, re-run rather than trusted

U6's verdicts were treated as prior claims to verify, not as truth. **All
seven reproduce**, including the one U6 predicted would survive. Each was run
after establishing a green baseline in the same environment, and each e2e
verdict below was confirmed to be an assertion firing — the failure text is
quoted — rather than the harness dying on a missing variable, which is the
shape that produced U6's six false KILLEDs.

| # | Mutation applied | Verdict | U6 said | Killed by |
| --- | --- | --- | --- | --- |
| M59 | `timeout-minutes: 10` removed from the `cli` job | KILLED | KILLED | `TestEveryJobIsTimeBounded`, `TestTheDeclaredTimeoutsAreTheOnesThePlanFixes` |
| M60 | e2e mints the key with `read,money:simulate` | KILLED | KILLED | `TestBroadcastExecutesAndRunsShowHasTheTokenScrubbed` and 3 others, on `403 INSUFFICIENT_SCOPE` |
| M61 | The `-X …version.Version` stamp dropped from `.goreleaser.yaml` | KILLED | KILLED | `TestTheReleaseStampsExactlyTheThreeVersionVariables` — **without goreleaser** |
| M69 | `after_record_written` asserts `Idempotency-Replayed` **present** | KILLED | KILLED | `TestResumeAfterACrashBeforeTheRequestLeftSendsItForTheFirstTime` |
| M71 | The release trigger changed from `v*` to `cli/v*` (run reversed, per A417) | KILLED | KILLED | `TestTheReleaseWorkflowTriggersOnlyOnVersionTags` |
| M116 | `http: {}` emitted on a local refusal | KILLED | KILLED | `TestAPATOnlyProfileIsRefusedBeforeAnyRequestLeaves` — **e2e only** |
| M117 | The `commands` row count `!= 1` weakened to `< 1` | **SURVIVED** | SURVIVED | — (equivalent; see below) |

Two of these have a narrower control than their §6.2 row suggests, and both
are worth stating because the consequence is the same: a CI run that omits
one step loses the row entirely.

- **M61 no longer needs goreleaser.** U6 fetched the 2.18.2 static binary to
  measure it. `e2e/release_config_test.go` reads `.goreleaser.yaml` directly
  and is in the default `go test ./...`, so the row now dies on a bare
  machine. The tagged `release_test.go` that actually builds is a second,
  stronger control, not the only one. A429.
- **M116 is killed by nothing but the end-to-end suite.** It survives all 26
  packages in both build configurations. CI's `cli-e2e` job is gated on
  `cli-e2e-preflight` and is skipped when `FERRY_API_REPO_SSH_KEY` is absent
  (A424), so on any run without that secret — a fork, or after a rotation —
  M116 and M60 have no control at all. That is the cost of A411's gate, now
  measured rather than reasoned about. A428.

**M117 survived, and the survival means nothing — it is an equivalent
mutant**, which is also what §6.2 predicted and what §6.3 says the row is for
("a demonstration, not a control"). Against a correct system
`commandsWithKey` returns exactly 1, so `!= 1` and `< 1` are the same
predicate and no input distinguishes them. That much is arithmetic. The part
worth measuring is whether the assertion is load-bearing in the direction it
stops covering — a count above 1 — and it is not: constructing that defect
(M3b's shape, a resume resending under `key + "-resumed"`) makes the resume
exit 4 and the test fatal at `resume_test.go:131`, the exit-code assertion,
before the count is ever consulted. The row count is defended in depth by
`assertKeyIs` and the exit code, which is why weakening it changes nothing.
A431.

### M70, which had a verdict and no row

§6.2's M70 ("make `--no-precheck` the default", AC63) was narrated in U6's PR
body and never tabulated, so a reader counting rows found nothing. Both forms
re-measured, and both reproduce U6's account exactly.

| # | Mutation | Verdict | Killed by |
| --- | --- | --- | --- |
| M70 | `OpSimulateTransfer` alone flipped to `RequireEither` | **SURVIVED the e2e suite**; KILLED by the unit census | `TestTheRequirementTableMatchesTheControllers` |
| M70b | All three money rows flipped (`simulate`, `execute`, `get_command`) | KILLED | `TestAPATOnlyProfileIsRefusedBeforeAnyRequestLeaves` — `http.status` 403 where null is required, and a run minted |

The reason M70 survives alone is the one U6 gave: the money path acquires a
`get_command` session alongside the money operation (`money.go:112`), and
`get_command` is independently `RequireAPIKey`, so a PAT-only profile is still
refused locally even with the simulate row opened. The behavioural control is
therefore only sensitive to all three rows together; the per-row control is
the table census, which is a different kind of test. Recording both is what
makes that legible. A432.

### What the §6.2 rows cite, and where the tests actually are

Seven of the rows measured here name an example file that does not exist or
does not hold the control. This is a documentation defect rather than a
coverage one — every row was killed — but it is the reason a reader
reconciling §6.2 against the tree concludes a row is uncovered when it is not,
which is the mistake this whole audit exists to stop. A430.

| # | §6.2 cites | The control is actually in |
| --- | --- | --- |
| M46, M47 | `broadcast_test.go` | `create_test.go` — no `broadcast_test.go` exists |
| M49 | `render_test.go` | `create_test.go` — no `render_test.go` exists |
| M111 | `execute_test.go` (pty) | `resume_test.go` — `execute_test.go` has no pty or `at_prompt` example |
| M59, M71 | `workflow_spec.rb` (the API repository) | `e2e/workflow_test.go`, moved here by A400 |
| M61 | `release_test.go` | `e2e/release_config_test.go` in the default suite; `e2e/release_test.go` behind `-tags goreleaser` is the second control |

### What the closure did not establish

- **M97.** Still unmeasurable from this repository, still A373, deliberately
  left rather than replaced with something that would reach a different
  branch and be recorded under its number.
- **That the 81 as-planned rows are true.** Unchanged from U7's caveat. This
  PR re-ran seventeen rows; it did not re-run the other hundred, and U6's own
  six false KILLEDs remain the reason to say that out loud.
- **Whether any surviving mutant exists outside §6.2.** The table is the
  hypothesis set. A defect nobody wrote a row for is not found by running the
  rows.

## U7's own mutations

Every assertion this unit wrote, measured. One mutation at a time, applied
with an occurrence-count assertion so a mis-anchored substitution cannot read
as a survivor, restored from a snapshot and `cmp`-verified byte-identical.

### AC66 — `internal/adversarial_test.go`

Run: `go test -count=1 ./internal/`.

| # | Mutation | File | Verdict | Killed by |
| --- | --- | --- | --- | --- |
| U7-1 | Delete §7.1's last row (A23) | `docs/PLAN.md:1849` | KILLED | the `minRows` floor, in every row test |
| U7-2 | Rename `TestA7TwoTerminalsResumingOneRun` so it stops naming a row | `adversarial_test.go` | KILLED | `TestTheAttackTableAndTheseTestsAreTheSameSet` |
| U7-3 | Point a citation at `TestLoadRefusesAFileWiderThan0644`, which does not exist | `adversarial_test.go` | KILLED | `TestA8ACredentialsFileAnyoneOnTheBoxCanRead` |
| U7-4 | `tagSets` → `{""}` only | `adversarial_test.go` | KILLED | six row tests whose citations live behind `faultinject` or `e2e` |
| U7-5 | `rowTestPattern` → `^TestA(\d)(.*)$`, dropping the `[A-Z_]` boundary | `adversarial_test.go` | KILLED | `TestTheRowIDPatternIsNotAmbiguous` |
| U7-6 | **Add** a 24th row to §7.1 with no test | `docs/PLAN.md` | KILLED | set equality, *forward* direction only |
| U7-7 | **Add** `func TestA24ATestForARowThatWasDeleted` with no row | `adversarial_test.go` | KILLED | set equality, *reverse* direction only |

U7-6 and U7-7 are the pair that matters, and they were built after a first
attempt failed to measure anything. Deleting a row (U7-1) is killed by the
`minRows` floor before either direction of the equality is consulted; removing
the reverse-direction loop *and* deleting a row is still killed by the floor.
Neither tells you the reverse direction is load-bearing. Adding a row keeps the
count at 24 ≥ 23 and the numbering contiguous, so only the forward check can
fire; adding a test does the same for the reverse check. U7-7 failed exactly
one example — `TestTheAttackTableAndTheseTestsAreTheSameSet` — which is the
direction the recurring one-directional-subset defect omits.

### AC67 / AC89 — `spec/docs/cli_docs_spec.rb` (in `coba-ai/ferry`)

Run: `FERRY_TEST_DATABASE=ferry_test_cli_u7 bundle exec rspec spec/docs/cli_docs_spec.rb`.

| # | Mutation | File | Verdict | Killed by |
| --- | --- | --- | --- | --- |
| M72 | Restore "No MCP server and no CLI. Both are planned; neither exists." | `docs/api/AGENTS.md` | KILLED | the absence-claim example and §1.4 item 8 |
| U7-8 | Delete the whole `## The CLI` section rather than correct it | `docs/api/AGENTS.md` | KILLED | the mentions floor and the link example |
| U7-9 | Link `cli/README.md` instead of the repository URL | `docs/api/AGENTS.md` | KILLED | the link example and the stale-path example |
| U7-10 | Restore `README.md`'s "there is no CLI binary yet … is planned" | `README.md` | KILLED | the absence-claim example and item 8 |
| U7-11 | Drop `default: 20` from the `Limit` parameter | `docs/api/openapi.yaml` | KILLED | §1.4 item 6 |
| U7-12 | Delete `ErrorDetails`' `additionalProperties: true` | `docs/api/openapi.yaml` | **SURVIVED — vacuous** | — |
| U7-12b | Strip both `additionalProperties` and `properties` from `RevokeApiKeyRequest` | `docs/api/openapi.yaml` | KILLED | §1.4 item 3 |
| U7-13 | `DESTINATION_TOO_NEW` marked retriable | `docs/api/errors.md` | KILLED | §1.4 item 1 |
| U7-14 | Rename the "Codes you will not see" heading | `docs/api/errors.md` | KILLED | §1.4 items 1b and 4 |
| U7-15 | `item3_defect?` returns a constant | `cli_docs_spec.rb` | KILLED | the put-back control |
| U7-16 | Delete item 7's entry from `defective_documents` | `cli_docs_spec.rb` | KILLED | "controls every claim it measures" |
| U7-17 | Broaden an absence pattern to `/\bCLI\b/` | `cli_docs_spec.rb` | KILLED | "does not call an accurate sentence an absence claim", ×3 |
| U7-18 | Delete §1.4 item 7 from `claims` | `cli_docs_spec.rb` | KILLED | "publishes exactly one still-present defect" |

**U7-12 is the one to read.** It edited the intended line — the occurrence
count confirmed it — and survived, which reads as a gap in the item 3
detector. It is not one. `ErrorDetails` also declares `properties`, so the
node was never "bare" and removing `additionalProperties` could not make it
so: the branch was unreachable by construction. That is the vacuous-control
class the plan warns about, arriving in the audit's own measurements. It is
recorded here rather than silently replaced, and U7-12b is the replacement
that reaches the branch.

U7-12 also produced the one real finding on this side. `ErrorDetails` is typed
`[object, "null"]`, not `object` — OpenAPI 3.1 allows a type list and this
contract uses it. The detector as first written matched only the scalar, so a
nullable response body with no shape would have walked straight past it. It
now reads `type` as a list. Re-measured with the broader detector the contract
is still clean, so item 3 is fixed in both spellings; had it not been, the
detector would have reported "fixed" over a document that was not. A374.

### AC70 / §7.1 A19 — the money-path gap

Already in the headline. Restated as a table, because the before/after is the
whole claim:

| Mutation | `main` | This PR |
| --- | --- | --- |
| `case false && run.TokenPrefix != "" && …` | SURVIVED | KILLED — `TestResumeRefusesACredentialWhoseTokenPrefixChanged` |
| principal arm → `outcome.ClassCLIFault` | SURVIVED | KILLED — `TestResumeRefusesAChangedCredential` |
| `case false && run.PrincipalID != "" && …` | KILLED | KILLED — `TestResumeRefusesAChangedCredential` |

The third row is the control: the principal arm always had an example, so its
mutation was killed before this PR too. Without it, the two survivors above
could be read as the whole of `checkCredential` being untested, which is not
what was found.

## What this audit did not establish

- **The 17 rows.** Not run *by U7*. They were listed rather than assumed, and
  the closure pass above has since run sixteen of them; M97 remains.
- **Whether a cited test asserts anything.** `internal/adversarial_test.go`
  binds each §7.1 row to tests by a name the toolchain resolves, so a rename
  or a deletion breaks the census. It does not re-run the cited assertion, and
  it cannot tell a strong test from a weak one. §6.2 is the control for that,
  which is why the 17 unreported rows mattered more than they would otherwise.
- **That the reported verdicts are true.** This is a reconciliation of what
  eight PRs said, not a re-run. U6's own report of five false KILLEDs — from a
  runner that read the exit code while the test binary was dying on a missing
  environment variable — is the reason to say so out loud. Any of the 81
  as-planned rows could have the same shape, and nothing here would show it.
