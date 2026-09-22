# The mutation audit

AC68. §6.2 of `PLAN.md` allocates 117 named mutations across 92 acceptance
criteria. This is the record of what was actually observed, reconciled from
the eight unit pull requests plus the follow-up fixes, and — where the record
was silent and the criterion was a money one — re-measured by U7.

§6.3 declares this document a **non-control**: its "mutation" would be
deleting it, and the value is the observations rather than a test that reads
it. Nothing here is asserted by a test, and that is deliberate. What *is*
asserted is `internal/adversarial_test.go`, which holds §7.1 to its tests, and
`spec/docs/cli_docs_spec.rb` in the API repository, which holds §1.4 to its
documents.

## The headline

**A money-path control was missing and is now written.** §6.2 M77 ("skip the
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
| Re-measured by U7 in this audit | 5 |
| Named in prose, never as a table row | 1 |
| **No verdict on record anywhere** | **17** |

The last row is the finding. An unreported mutation is not a mutation that
passed; it is a mutation nobody ran, and M77 is the proof that the distinction
has teeth.

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

`kurenn/ferry` #18 (AC85, "type the five response bodies") numbered its
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

### Named in prose only (1)

**M70** (make `--no-precheck` the default, AC63). U6 narrates it in the PR
body — it survived when only `OpSimulateTransfer` was flipped, because the
money path independently acquires an api-key-only `get_command` session, and
died when all three rows were flipped. There is no table row, so a reader
counting rows finds nothing. The verdict exists; the record of it does not.

### No verdict on record anywhere (17)

| # | Mutation | AC | Whose |
| --- | --- | --- | --- |
| M42 | Usage error to stderr only in JSON mode | AC41 | U5 |
| M45 | Store the token on a simulate-only run | AC46 | U5 |
| M46 | Re-simulate on `PLAN_EXPIRED` | AC47 | U5 |
| M47 | Mint the execute key after the simulate 201 | AC47 | U5 |
| M49 | Print `✓ success` | AC49 | U5 |
| M59 | Remove `timeout-minutes` from `cli` | AC60 | U6 |
| M60 | e2e scopes `read,money:simulate` | AC61 | U6 |
| M61 | Drop `-X main.version` | AC64 | U6 |
| M69 | `after_record_written` asserts `Idempotency-Replayed` present | AC62 | U6 |
| M71 | Trigger `cli-release.yml` on `v*` | AC65 | U6 |
| M86 | Flag body through `map[string]any` with a random key | AC78 | U5 |
| M87 | `after_simulate_recorded_before_execute_begin`: resume re-simulates | AC80 | U5 |
| M88 | `after_simulate_send_before_record`: resume executes with a `null` token | AC80 | U5 |
| M97 | Delete a still-present defect from `DOC-DEFECTS.md` | AC89 | U7 — see below |
| M111 | `transfers execute` writes `awaiting_confirmation` only on `y` | AC93 | U5 |
| M116 | AC63 e2e: emit `http: {}` on a local refusal | AC63 | U6 |
| M117 | AC62 e2e: skip the `commands` row count | AC62 | U6 |

Eight of the seventeen are U6's, and the reason is structural rather than
careless: **U6's PR body reports a count and not a table.** "25 applied. 21
KILLED, 2 SURVIVED, 2 superseded by a corrected application" — with the five
false KILLEDs it caught described at length, which is the most valuable
paragraph in any of the eight PRs. But no row is attributable to a §6.2
number, so eight rows covering CI, the e2e suite and the release cannot be
reconciled. Recorded as A371.

Seven of the remaining nine are U5's, and five of those seven are money-path
(M45, M46, M47, M86, M87, M88, M111). U5 applied 30 mutations and reported
every one; these are simply rows it did not reach. Given M77, they should not
be assumed green. Recorded as A372. U7 did not run them: the protocol is one
mutation at a time with a restore and a `cmp` between each, and seventeen of
those is a unit of work rather than the tail of an audit. Saying so is the
honest answer; running five of them and implying the rest would not be.

**M97 is U7's own and is only half-measurable.** It says "delete a
still-present defect from `DOC-DEFECTS.md`" and expects `cli_docs_spec.rb` to
go red. `DOC-DEFECTS.md` is in this repository and the spec is in the other
one, and the spec deliberately does not read across that boundary. The half
that is measurable — delete a claim from the spec's own §1.4 set — was applied
and killed. The half that is not is A373.

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

### AC67 / AC89 — `spec/docs/cli_docs_spec.rb` (in `kurenn/ferry`)

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

- **The 17 rows above.** Not run. Listed, not assumed.
- **Whether a cited test asserts anything.** `internal/adversarial_test.go`
  binds each §7.1 row to tests by a name the toolchain resolves, so a rename
  or a deletion breaks the census. It does not re-run the cited assertion, and
  it cannot tell a strong test from a weak one. §6.2 is the control for that,
  which is why the 17 unreported rows matter more than they would otherwise.
- **That the reported verdicts are true.** This is a reconciliation of what
  eight PRs said, not a re-run. U6's own report of five false KILLEDs — from a
  runner that read the exit code while the test binary was dying on a missing
  environment variable — is the reason to say so out loud. Any of the 81
  as-planned rows could have the same shape, and nothing here would show it.
