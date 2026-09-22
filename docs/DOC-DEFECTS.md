# Documentation defects still present

AC89. `PLAN.md` §1.4 is a table of defects found in `coba-ai/ferry`'s
`docs/api/` during the CLI planning pass. This file lists the ones **still
there**, and nothing else.

A list of fixed defects is worse than no list: it is accurate the day it is
written and fiction a month later, because nothing re-reads it. So the
authority is not this file — it is
[`spec/docs/cli_docs_spec.rb`](https://github.com/coba-ai/ferry/blob/main/spec/docs/cli_docs_spec.rb)
in the API repository, which measures every §1.4 item against the document it
is about and fails in both directions: a row it records as fixed that the
measurement finds again is red, and a row it records as still present that the
measurement cannot find is also red, saying to take it off this list.

This file is the human-readable half of that. If the two disagree, the spec is
right.

## Why the list and the measurement are in different repositories

A400 moved the CLI, this plan and this file into `coba-ai/ferry-cli`. The
documents they are *about* stayed in `coba-ai/ferry`. A spec here would have to
reach into a sibling checkout to read them, which is the coupling A400 existed
to remove and which A404 showed breaks in a worktree.

So **nothing enforces that this file agrees with the spec** (A373). The spec's
final example publishes the still-present set as a single assertion — changing
it means changing this file in the same breath — but that is a convention, not
a check. §6.2's M97 ("delete a still-present defect from `DOC-DEFECTS.md`")
expects a red spec and cannot get one; the half of it that is measurable, a
claim deleted from the spec's own set, was applied and killed.

## Still present — one item

### Item 7 — "an unrecognised key is refused" is stated for the whole API, and execute does not do it

**Where:** `docs/api/AGENTS.md`, the idempotency section, in the phrase
"as everywhere else in this API".

**What is wrong:** the page states, as a property of the API, that an
unrecognised key in a request body is refused with `400 VALIDATION_FAILED`
rather than ignored. It is true of the api-key endpoints, whose schemas are
`additionalProperties: false`. It is **not** true of `POST /v1/transfers`:
execute digests unknown keys into the idempotency fingerprint and otherwise
ignores them.

**Why it is not fixed here:** the sentence is a correct description of most of
the API and the fix is to narrow it, which is a documentation change in
`coba-ai/ferry` that U7 could make. It is left open deliberately, because the
alternative reading — that execute *should* refuse unknown keys, and the
documentation is describing the intended behaviour — is a money-path API
change and not U7's to decide. P2 recorded the measurement; the decision is
still open.

**Why it matters to a client:** a caller who trusts the sentence will send a
misspelled field to execute and expect a `400`. It gets a `201`, and the
misspelled field is in the fingerprint, so the corrected retry is a *different*
body under the same key and answers `409 IDEMPOTENCY_KEY_REUSED`. The
documented behaviour and the real one differ exactly where the cost of the
difference is highest.

**How it is measured:** `cli_docs_spec.rb`'s `item7_defect?`, which looks for
the phrase in `AGENTS.md`. When somebody narrows the sentence, that example
goes red and says to delete this section.

## Fixed, and re-measured so they stay fixed

Not listed here, per AC89 — this file is the still-present list. They are named
because "fixed" is a claim the spec re-checks every run, and a reader who finds
only one item should know the other eight are watched rather than forgotten:
items 1, 1b, 2, 3, 4, 5, 6 and 8, each with a detector and each with a control
that drives the detector over a document with the defect put back.

Two are worth a sentence:

- **Item 3** (response bodies typed as a bare `{ type: object }`) was gated in
  §1.4 on "until U8 lands". U8 landed. It was re-measured rather than trusted,
  by walking every node in `openapi.yaml`, and it is genuinely fixed — for
  nullable bodies too, which the first version of the detector would have
  missed (A374).
- **Item 8** (both documents said the CLI does not exist) is what this unit
  fixed, and the spec matches *claims* rather than the sentence that was
  there, so a rewrite to "the CLI is not built yet" is the same defect and
  still red. It also enforces a floor on how many lines mention the CLI at
  all, because deleting the paragraph would otherwise satisfy every check on
  this criterion.
