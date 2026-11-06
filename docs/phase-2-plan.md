# Dataset Studio: Phase 2 Plan, Dataset Production (Revision 1)

This document describes Phase 2: turning source material into traceable,
validated datasets for training and evaluating AI models. Research papers are
the first source type.

> **This document gives no authority to implement anything.**
>
> - Its architecture is **Approved direction**: the owner has accepted the
>   direction, and nothing more.
> - Every milestone in it is **Proposed**.
> - A Phase 2 milestone may be implemented only after its own **milestone
>   authorisation** is merged (section 0.2 and `CLAUDE.md` section 5).
> - The Phase 1 plan, [`docs/phase-1-plan.md`](phase-1-plan.md) (Revision
>   2.2), is unchanged by this document and always takes precedence over it.

| Revision | Content | Approval status |
|---|---|---|
| 1 | Product direction, layered architecture, knowledge identity, LLM call lifecycle, rights and data governance, proposed milestones | Decisions R3-AP1 to R3-AP7 (section 20.1) approved by Godwin, 2026-11-06, at the levels stated there. Everything else is Proposed. |
| 1, build-readiness corrections | Section 6 corrections C6-1 to C6-12; contradictions and gaps X-A to X-L; Phase 2 dependency table, orchestration and alerts (section 3); acceptance criteria P2-AC7 to P2-AC12; milestone N0; P2-X12 resolved; EXP-1 deferred; new Unresolved items P2-X25 to P2-X35 | Incorporation approved by Godwin, 2026-11-07, before this document's first merge. Each correction keeps the label of the section it amends. Recommendations that need an owner or legal decision stay **Unresolved** and are marked as such (section 20.2). |

---

## 0. Status labels and authorisation

### 0.1 Labels

This plan uses the Phase 1 labels
([Phase 1 section 0](phase-1-plan.md#0-status-labels)) and adds two.

| Label | Meaning |
|---|---|
| **Approved direction** | The owner has accepted this architectural direction. It is **not** authority to implement. |
| **Proposed** | Not approved. Recorded for review. |
| **Assumption** | As in Phase 1: it holds until changed and is never turned into a guarantee. |
| **Unresolved** | Not decided. It must be decided before the milestone that needs it (section 20.2). |
| **Verified** | As in Phase 1. **Nothing in this document is Verified yet.** In particular, no provider capability has been verified. |

### 0.2 Authorisation model (Approved direction, R3-AP3)

- Standing authorisation in `CLAUDE.md` covers Phase 1 milestones M1 to M14
  only.
- Each Phase 2 milestone needs a **milestone authorisation**: a document
  Godwin has merged, stating the milestone's scope, acceptance criteria, test
  ids, database grants (in the form of Phase 1 section 6.3), API endpoints,
  new dependencies (section 3.1), security review items, and the Unresolved
  items it settles. This plan specifies no Phase 2 endpoint; each
  authorisation fixes its own, following the Phase 1 API conventions (Phase 1
  section 15) (X-I).
- **Written authorisation** means a merged document with an explicit scope. A
  chat message, a PR comment or an unmerged draft is not written
  authorisation.
- An experiment needs its own scoped written authorisation, separate from any
  milestone authorisation (section 16.3).

---

## 1. Purpose and product direction (Approved direction, R3-AP2)

Dataset Studio becomes a general-purpose dataset production platform. It turns
source material into high-quality, traceable, validated datasets suitable for
training and evaluating AI models.

The 11 product decisions accepted as direction:

1. Dataset Studio is a general-purpose dataset production platform.
2. Research papers are the first supported source type, not the permanent
   product boundary.
3. The architecture supports additional source types through adapters.
4. A reusable canonical knowledge layer is separate from dataset records.
5. Dataset records are derived projections, not the source of truth.
6. Knowledge revisions, evidence, derivations and provenance are preserved.
7. Extracted, inferred, generated, curated and imported content remain
   distinguishable.
8. Go, PostgreSQL and the modular-monolith architecture are retained.
9. No graph database or vector database is introduced without demonstrated
   need.
10. The separate Senebiclabs expert-answering system is not integrated.
11. The Senebiclabs website and dataset catalogue are not built as part of
    this project.

The production lifecycle this plan addresses: source ingestion, content
understanding, exhaustive knowledge extraction, knowledge structuring, derived
content generation, knowledge-gap discovery, dataset construction, quality
assurance, and versioned release.

---

## 2. Relationship to Phase 1 (Approved direction, R3-AP1)

- **Phase 1 is unchanged.** Its scope, exclusions (Phase 1 section 18),
  assumptions (section 20.2), acceptance criteria (section 17) and dependencies
  (section 16) apply exactly as written. Phase 1 remains Revision 2.2.
- **Phase 1 rules apply to all Phase 2 code**, without exception:
  database identities and grants (Phase 1 section 6), principals and authorship
  (section 7), audit semantics (section 8), security (section 14), and the
  cross-cutting tests (section 17.3).
- **Phase 2 builds on Phase 1 milestones**; the dependencies are in section 16.
- **The two Phase 1 tests that no milestone schedules,** the OpenAPI contract
  test (Phase 1 section 17.3 #4) and the neutrality guard (#5), remain Phase 1
  items. This plan does not assign, move or remove them. Scheduling them (M14
  is the recommended place) is a Phase 1 decision for Godwin, outside this
  document (X-I).
- **Phase 1 rights and freeze behaviour is unchanged.** Phase 2 adds purposes,
  use contexts and gates only for Phase 2 operations and Phase 2-derived
  release members (sections 7.2, 7.4 and 14; X-A, X-B, X-C).

**Informational: what Phase 2 will need from Phase 1 (non-binding on
Phase 1).** These notes impose no Phase 1 obligation. If honouring one would
change Phase 1 scope, that needs a Phase 1 plan amendment.

| Phase 1 area | Phase 2 need | Why it is expected to be additive |
|---|---|---|
| M8, M9 registry | New registry kinds (section 9) | Phase 1 section 2.1: a new kind is one handler plus one meta-schema, with no table changes |
| M11 rights | Purpose vocabulary and closure evaluation (section 7) | Phase 1 section 5.6: purposes are already vocabulary concepts |
| M12 records | Tables that reference record revisions (`record_derivations`) | New tables only |
| M13 releases | Manifest and bundle versions above 2; extra freeze gates | Phase 1 section 9.5 already has `bundle_format_version` |
| M6 jobs | Timeouts per job kind | Phase 1 already gives release jobs their own timeout |

---

## 3. Layers and boundaries (Approved direction for the layering; detail Proposed)

```
 Platform (Phase 1): identity · authz · audit · jobs (River) · blobs · backup · registry
 L1 Sources       sources, artifacts, metadata, rights, provenance        (Phase 1, M11)
 L2 Content       content versions, content units, locators               shared model
      adapters    research-paper PDF first; others later                  source-type specific
 L3 Knowledge     units, revisions, anchors, evidence, relations,
                  derivations, checks, identity decisions                 shared
 L4 Production    pipeline runs and steps · LLM gateway and call ledger   shared
      stages      extract · verify · consolidate · gaps · generate        shared engines, configured
                                                                          by registry profiles
 L5 Datasets      transform specs → records (projections) · serializers   shared
 L6 Quality and release   checks, quality profiles, freeze gates,
                  manifest, bundle                                        shared (extends Phase 1 M13)
```

- **Source-type-specific behaviour** lives only in adapters, chosen by adapter
  id or media type, never by dataset type, and in registry profiles.
- **Parsers not written in Go** run as pinned tool adapters invoked by the
  worker, not as services. The tool choice is Unresolved (P2-X1).
- **No microservices.** The modular monolith, PostgreSQL and River are
  retained.

### 3.1 Phase 2 dependencies (Proposed; X-D)

Phase 1 dependencies stay in Phase 1 section 3.1. A dependency that only a
Phase 2 milestone needs is recorded here, with its pin, release date and
licence, and is approved through that milestone's authorisation. The same
rules apply as in Phase 1: pinned, at least two weeks old at pinning, and
clean under govulncheck.

| Component | Pin | Status |
|---|---|---|
| Parser tool for research-paper PDFs | — | **Unresolved** (P2-X1). Candidates must be evaluated for licence (AGPL needs an explicit owner decision), table fidelity, coordinates and determinism. No candidate is verified. |
| PDF writer for synthetic test fixtures | — | Proposed for N1, chosen in the N1 authorisation. Fixtures are synthetic only (GOV-9). |
| LLM provider clients | none | Proposed: the standard library `net/http`, no provider SDK. |
| Columnar serializers (for example Parquet) | — | Proposed for after N8's JSONL serializer (P2-X7). |

### 3.2 Pipeline orchestration (Proposed; X-H)

- `pipeline_runs`: one requested run, its pinned registry versions (profiles,
  prompt templates, schemas), pinned model ids, budget ceiling, requesting
  principal and status.
- `pipeline_steps`: one row per step and input batch, with its purposes
  (GOV-2), its River job id and its outcome.
- One River job kind per step type. Every step uses the Phase 1 fence (Phase 1
  section 12.1) and is idempotent; a resumed run re-enqueues only steps
  without a committed outcome.
- Fan-out is by batches of content units. Cancellation sets the run
  `cancelling` and is honoured at each step's fence.
- Registry kinds added by Phase 2, each one handler plus one meta-schema
  (Phase 1 section 2.1): `prompt_template`, `extraction_profile`,
  `verification_profile`, `generation_profile`, `aspect_checklist`,
  `transform_spec`, `quality_profile`, plus a declared knowledge-type role on
  `record_schema`.
- The design is fixed in the N4 milestone authorisation.

### 3.3 Alerts and operational status (Proposed; C6-10)

- An **alert** is an append-only `alerts` row (kind, subject, details, raised
  at) written in the same transaction as the event that raised it, with that
  transaction's audit event. While any alert is unacknowledged, `/readyz`
  reports degraded, and an ERROR log line is written. Acknowledging an alert is
  an audited command that writes an append-only acknowledgement row; it never
  updates or deletes the alert.
- External paging is out of scope (deployment is excluded, section 18).
- Status endpoints read the database: runs, steps, costs against ceilings,
  alerts. No metrics stack is added.
- The worker checks free disk space for the blob root and spool at start-up
  and before each send, and refuses to proceed below a configured threshold.

---

## 4. Content model (Proposed)

| Table | Kind | Content |
|---|---|---|
| `content_versions` | Immutable once complete | id, source_id, artifact-set sha256, adapter id and fingerprint, status |
| `content_units` | Immutable | id, version_id, parent_id, ordinal, `unit_type`, text, text_sha256, locator (jsonb), attributes (jsonb), confidence, `derivation` |

- `unit_type` is a closed set of structural primitives: `container`, `heading`,
  `text`, `list_item`, `table`, `table_cell`, `figure`, `caption`, `equation`,
  `code`, `reference`, `data_record`, `data_field`. These describe structure,
  not domain or dataset type.
- `derivation` ∈ {`native`, `ocr`, `vision`, `converted`}.
- Units are blocks. Sentence-level citation uses character spans inside a block.
- Table cells carry their row and column header paths in `attributes`.
- Each adapter declares a locator schema (for example page and bounding box for
  PDF, row and column for tables, JSON Pointer for JSON). Proposed for the PDF
  adapter: 1-based page, bounding box in PDF points, and the unit's ordinal
  path.
- Character spans count Unicode code points over NFC-normalised unit text; the
  normaliser version is part of the adapter fingerprint.
- Parsing is deterministic: the same artifact set and adapter fingerprint
  produce a byte-identical canonical representation (a CU test).
- The full representation is also stored as one canonical JSON blob.
- Bulk exports of content units or knowledge are data egress and are audited,
  as Phase 1 approved default U3 does for artifacts; single reads are not.

---

## 5. Knowledge model and identity (Approved direction, R3-AP6)

### 5.1 Tables

| Table | Kind | Content |
|---|---|---|
| `knowledge_units` | Mutable status only | id, project_id, type entry id, current_revision_id, status (`candidate`, `accepted`, `rejected`, `merged_into`, `superseded`, `needs_review`), merged_into_id (set exactly when status is `merged_into`) (X-E) |
| `knowledge_unit_revisions` | Immutable | id, unit_id, seq, payload (jsonb), payload_sha256, schema version id, `origin`, `epistemic_status`, statement, producing step id, llm_call id, validation report |
| `evidence_anchors` | Immutable | id, content_version_id, content_unit_id, char_start, char_end, anchor fingerprint (normalised span hash plus context hash) |
| `anchor_mappings` | Append-only | old anchor, new anchor, method (`exact`, `normalized`, `fuzzy`), confidence, identity decision id |
| `knowledge_evidence` | Immutable | revision_id, anchor_id, role (`supports`, `qualifies`, `contradicts`, `context`) |
| `knowledge_relations` | Immutable, superseded by later rows | from_unit, to_unit, relation type (vocabulary concept), origin, evidence |
| `knowledge_derivations` | Immutable | derived_revision_id, premise_revision_id, role |
| `knowledge_checks` | Append-only | subject ids, check kind, checker fingerprint, outcome, score, details, `contains_source_content` |
| `identity_decisions` | Append-only | see section 5.5 |
| `extraction_candidates` | Immutable | id, run, step, ordinal, llm_call id, candidate payload, payload_sha256; the subject of identity decisions (section 5.3) (X-E) |
| `identity_rules` | Synchronised by migration from a catalogue in code | rule_id, rule_version, active, allowed_actions, required checker fingerprints (section 5.5, T2 and T3) (X-E) |
| `record_derivations` | Immutable | output record revision, knowledge or gap revision, transform spec version |

- `origin` ∈ {`extracted`, `inferred`, `generated`, `curated`, `imported`}.
  `imported` is reserved for externally obtained content; no integration is
  designed.
- `epistemic_status` ∈ {`asserted`, `hedged`, `speculative`, `conditional`,
  `attributed`}. `attributed` marks a claim the source reports from another
  work.
- Qualifications (population, condition, timeframe, quantity) stay inside the
  atomic unit as payload fields. Relations link separate claims.
- Knowledge-type payload schemas are registry `record_schema` entries with a
  declared role. Each schema marks its identity-bearing fields with an
  annotation (`x-studio-identity`).

### 5.2 Quantities

A `quantity` holds:

| Part | Example | Identity-bearing? |
|---|---|---|
| `lexical` | `"0.050"`, exactly as in the source | No; subject to fidelity checks |
| `value` | `"0.05"`, canonical decimal string | **Yes** |
| `reported_precision` | 3 decimal places, derived from `lexical` | No (Unresolved, P2-X17) |
| `unit`, `comparator`, `uncertainty` | `mg`, `<`, `{kind, low, high, level}` | **Yes** |

Numbers are decimal strings, never JSON numbers, because RFC 8785
canonicalisation would rewrite `0.050` as `0.05`.

### 5.3 Three kinds of identity

| Identity | Defined by | Mutable? |
|---|---|---|
| Evidence anchor | Content version, content unit and span, plus a version-independent anchor fingerprint | Immutable |
| Candidate | One extraction output: run, step, ordinal, llm_call | Immutable; never merged or edited |
| Knowledge unit | A stable id, assigned only by an identity decision | Its revisions are immutable |

Principles:

- Two units share an identity only when they express the same proposition,
  with the same truth conditions, from the same source.
- Similar wording is never sufficient.
- Identity changes only through a recorded decision.
- When in doubt, keep units apart and link them: a wrong split is recoverable,
  a wrong merge is not.
- Releases pin revision ids and anchor ids, never unit ids, so later identity
  decisions cannot change released data.

### 5.4 Anchor updates versus knowledge revisions

- Re-anchoring after a parser upgrade writes new anchors and `anchor_mappings`.
  It **never** creates a knowledge revision.
- A new knowledge revision is created only when (a) the payload changes
  through a recorded `improvement`, `fidelity_correction` or
  `lexical_fidelity` decision, or (b) a decision changes the evidence set.
- A later run that reproduces a unit records a `reproduction` decision and
  creates no revision.
- Parser upgrade outcomes: identical text gives an `exact` mapping;
  text equal after normalisation gives a `normalized` mapping; changed text
  gives `anchor_drift` and sets the unit to `needs_review`; no match gives
  `anchor_lost` and sets the unit to `needs_review`. Units are never deleted
  and never silently re-pointed.

**Fidelity correction (KI4).** A `revision_of(fidelity_correction)` is allowed
only when all of the following hold, recorded in one `fidelity_verification`
check (checker fingerprint, span hash, matched token offsets):

1. The old and new revisions share an anchor id, or are linked by an `exact`
   mapping.
2. The corrected field has a deterministic verifier (quantities only, for now).
3. The anchor's content unit has `derivation = native`.
4. Exactly one numeric token in the span matches the new value lexically, with
   matching unit, comparator and table header path.
5. The old value matches no token in that context.
6. All other identity-bearing fields are equal.
7. No other candidate token in the span could match. Otherwise the case goes
   to review.

### 5.5 Identity decisions and database enforcement

`identity_decisions` (append-only; UPDATE and DELETE rejected) records the
candidate, the action, the decider, the signals used and a reason. Actions:
`new_unit`, `reproduction`, `revision_of`, `duplicate_of`,
`possible_duplicate`, `possible_conflict`, `held_for_review`,
`not_reproduced`, and the principal-only actions `merge`, `split`, `unmerge`,
`accept`, `reject`.

An LLM's equivalence judgement is stored only as a `knowledge_check` signal.

On every INSERT, triggers enforce:

| # | Check |
|---|---|
| T1 | `decider_kind ∈ {rule, principal}` (CHECK). |
| T2 | **Rule authorisation.** `(rule_id, rule_version)` exists in `identity_rules` (a catalogue in code, synchronised by migration), is active, and the action is in that rule version's `allowed_actions`. `merge`, `split`, `unmerge`, `accept` and `reject` are in no rule's set. |
| T3 | **Required checks for each action.** Each referenced check exists, belongs to the decision's candidate and target, has outcome `pass`, uses the checker fingerprint the rule version requires, and was recorded before the decision. `reproduction`, `duplicate_of`: `identity_blockers` and `anchor_agreement`. `revision_of(improvement)`: those two and `improvement_criteria`. `revision_of(fidelity_correction)`: `fidelity_verification`, `anchor_agreement(exact)` and `identity_blockers` excluding the corrected field. `revision_of(lexical_fidelity)`: `lexical_fidelity`, `anchor_agreement(exact)` and `identity_blockers`. `new_unit`, `possible_duplicate`, `possible_conflict`, `held_for_review`, `not_reproduced`: no pass required, because none of them combines identities. |
| T4 | **Audit link.** `audit_event_id` is NOT NULL and a foreign key. A rule decision's event is a `job_execution` event whose `job_id` is the producing job. A principal decision's event actor is that principal and its action is an `identity.*` catalogue action. `created_by` equals the event actor (Phase 1 section 7.2). |
| T5 | **State consistency.** The target unit's status permits the action. Each candidate gets one final decision; a reversal references the decision it reverses. |

Identity-blocking differences (deterministic): knowledge type, source,
epistemic status or attribution, polarity or negation, quantity `value`,
`unit`, `comparator` or `uncertainty`, and any other identity-bearing
qualifier.

Inferred and generated units are keyed by premise revision set, generation
profile version, purpose and content hash. They are never merged on meaning.

**Review queues (Proposed; X-J).** Principal actions (`accept`, `reject`,
`merge`, `split`, `unmerge`) and operator resolutions (section 6.6) are worked
from queues read through the API and `studioctl`: units in `needs_review`,
candidates `held_for_review`, gaps awaiting triage, and calls in
`needs_operator`. Phase 1 review profiles are still not executed. Whether a
web UI is added, and the review sampling rate, are Unresolved (P2-X5).

### 5.6 Knowledge identity tests (KI, Proposed; not implemented)

| ID | Case | Expected |
|---|---|---|
| KI1 | Different wording, same proposition, same span | `reproduction`; no revision |
| KI2 | Same proposition restated elsewhere in the source | `possible_duplicate` and `held_for_review`; no automatic merge |
| KI3 | Same wording, different sources | Separate units |
| KI4 | Different numeric value, same span | Not merged. `fidelity_correction` only under the seven conditions in section 5.4; otherwise `held_for_review` |
| KI5 | Different comparator or unit (p<0.05 vs p=0.05; 5 mg vs 5000 µg) | New unit or held; no automatic conversion |
| KI6a | Same anchor, "0.05" vs "0.050", source reads "0.050" | Same identity; `lexical_fidelity` revision if the existing lexical form mismatches the source, otherwise `reproduction` |
| KI6b | "5%" vs "0.05" | Units differ; `held_for_review` |
| KI6c | Lexical form does not match the span | `lexical_fidelity` check fails; unit flagged; not an identity blocker |
| KI7 | Different population | New unit |
| KI8 | Different timeframe | New unit |
| KI9 | Different attribution (own finding vs reported from another work) | New unit |
| KI10 | Different polarity | New unit |
| KI11 | Different hedging | New unit |
| KI12 | Claim splitting: existing "A reduced B and C", candidate "A reduced B" | New unit with a `part_of` link; only a principal can split the existing unit |
| KI13 | Claim merging: existing "A→B" and "A→C", candidate "A→B and C" | New unit with `possible_duplicate` links; only a principal can merge; unmerge restores both |
| KI14 | Parser change, identical text | `exact` mapping; no revision |
| KI15 | Parser change, text equal after normalisation | `normalized` mapping; no revision |
| KI16 | Parser change, text changed | `anchor_drift`; `needs_review`; no automatic revision |
| KI17 | Parser change, span lost | `anchor_lost`; `needs_review`; never deleted |
| KI18 | New model reproduces the unit | `reproduction`; no revision |
| KI19 | New model differs in an identity field | New unit, plus `possible_conflict` if anchors overlap |
| KI20 | New prompt improves a non-identity field | Revision only through a recorded `improvement` decision |
| KI21 | Unit not reproduced by a newer run | `not_reproduced` check; unit unchanged |
| KI22 | LLM signal says "same" but a blocker fires | No merge |
| KI23a | Raw-SQL decision with `decider_kind = llm` | Rejected (T1) |
| KI23b | Rule decision with an action outside the rule's `allowed_actions`, or an inactive rule | Rejected (T2) |
| KI23c | Rule merge without passing required checks, or with checks for another subject or stale fingerprint | Rejected (T3) |
| KI23d | Decision without an audit link, or with a mismatched event | Rejected (T4) |
| KI23e | Action not permitted by the target's status; second final decision for a candidate | Rejected (T5) |
| KI24, KI25 | Unassigned. These numbers are deliberately unused (X-F). | — |
| KI26 | Property test: mutate each identity-bearing field across all corpus pairs | Zero merges |
| KI27 | Merge, split or re-anchor after a release | Rebuilt bundle is byte-identical |
| KI28 | Generated items with the same premises but different profiles | Separate units |

---

## 6. Production and the LLM call lifecycle (Approved direction, R3-AP4)

R3-AP4 approves this section as architectural direction, subject to the
filesystem qualification in section 6.9. Provider-specific parts depend on
P2-X2. The Unresolved items P2-X2, P2-X14, P2-X15, P2-X21, P2-X22, P2-X23,
P2-X24, P2-X27 and P2-X31 are listed where they apply and are not decided
here.

Corrections C6-1 to C6-12 (approved for incorporation on 2026-11-07) are
marked where they apply. They tighten the design. They do not change the
evidence taxonomy, the operator-resolution model or Phase 1 section 8.1.

### 6.1 Three separate counters

| Concept | Recorded in | Used for |
|---|---|---|
| River job attempt | `river_job.attempt` | Fencing the job's database commit only. Never used as a call's submission count. |
| Call | `llm_calls` (immutable intent) | One logical request: request sha256, request blob, provider, pinned model, prompt template version, parameters, input content-unit ids, budget ceiling |
| Call submission | `llm_call_submissions` (one row per send attempt, own `seq`) | The call's durable, independently recoverable attempt history |

Supporting tables: `llm_call_consumptions` (unique on `call_id`),
`llm_call_resolutions` (immutable, one per submission),
`llm_call_resolution_proposals` (append-only, section 6.6),
`llm_provider_capabilities` (section 6.5).

### 6.2 Submission lifecycle

1. **Reserve.** A reserve job, or the pipeline step that creates the call
   intents, commits one `llm_call_submissions` row per call (`state =
   reserved`, idempotency key, reserved budget) in a fenced transaction with
   one `job_execution` audit event, and enqueues the send job. **No call is
   sent without a committed, audited reservation for that exact
   `(call_id, seq)`.**
2. **Send.** The send job handles its reservations one call at a time
   (section 6.3).
3. **Record.** After the batch, or when its time budget runs out, the send
   job's fenced transaction records each reached submission's outcome
   (`responded`, `not_sent`, `failed_not_executed`, `failed_charged`,
   `ambiguous`) with usage and any provider request id, writes one audit
   event, and enqueues consumption for responded calls. Calls not reached stay
   `reserved` with no marker and remain safely sendable.
4. **Resolve ambiguity** (section 6.4). An ambiguous submission is never
   resent under the same `seq`.
5. **Reconcile.** A periodic reconcile job finds reservations whose lease has
   expired with no recorded outcome. The lease is at least
   `T_max + 2w + margin` (C6-7). For each call, the reconciler first tries the
   advisory lock `llm_call_key(call_id)` and **skips the call if the lock is
   held**, because a live sender still owns it; sealing a healthy send would
   make it ambiguous for no reason (C6-7). Otherwise it seals and inspects the
   call's spool slot independently (section 6.8), records outcomes, and writes
   a periodic `job_execution` event.

The send batch size is Unresolved (**P2-X14**).

### 6.3 Sending, the advisory lock and lock loss

**Lock ownership and execution certainty are different things.**

- Holding the session advisory lock is a database fact. It holds only while
  the session holding it is alive, and can be lost silently.
- It only stops two senders that both still have live sessions from starting
  sends for the same call at the same moment.
- Whether the provider executed a request can be established only by a
  published response, a documented and reliable status lookup, documented
  idempotency semantics, or authoritative evidence (section 6.6).
- **The advisory lock never guarantees exclusive external execution.**

Sender behaviour for submission `(call, n)`:

1. Acquire the session lock `llm_call_key(call_id)` with try-lock, on a
   dedicated connection with no open transaction. If it is held elsewhere,
   skip the call.
2. Start a watchdog that runs a liveness query on that connection every *w*
   seconds. Any error, timeout or dropped connection counts as lock lost.
3. Create the `intent` marker in the call's spool slot with `O_CREAT|O_EXCL`,
   fsync the file and the directory. **If this fails, do not send.**
4. Send under a context that is cancelled on lock loss and that has a hard
   deadline `T_max`. *w* and `T_max` are Unresolved (**P2-X21**).
5. On lock loss: cancel the request; write a `lock_lost` marker if possible;
   send nothing further in this job attempt; return an error, never `nil`
   (Phase 1 section 12.1). Cancellation cannot prove non-execution, so the
   submission stays ambiguous.
6. A response that still arrives after lock loss is published if the slot
   allows (section 6.8), otherwise written to `late/`, and marked
   `received_after_lock_loss`.
7. A request rejected before any byte is written is `not_sent`. A request
   that reaches `T_max` is ambiguous.

**The `lock_lost` marker is supplementary evidence only.** Classification
never reads it. A submission is ambiguous exactly when an `intent` marker
exists, there is no verified publication, and there is no recorded
`not_sent`. If writing or fsyncing `lock_lost` fails, the sender still cancels
and returns an error.

### 6.4 When a new submission may be created

A trigger on INSERT into `llm_call_submissions` allows `n+1` only when
submission `n` is:

| State of `n` | Allowed when |
|---|---|
| `not_sent` | no further condition |
| `failed_not_executed` | the capability record documents that this error means the request did not execute |
| `resolved_not_executed` | established under section 6.6, through documented lookup guarantees or authoritative evidence |
| `ambiguous` or `needs_operator`, through the **same-key retry path** | all conditions in section 6.5 hold |

`idempotent_retry` is a **creation path** for `n+1`, recorded on `n+1`; it is
not a state of `n` (C6-6). The complete set of submission states and
transitions is in section 6.8.

In every other case, including `ambiguous` with no documented idempotency or
lookup guarantee, the reconciler moves the submission to `needs_operator`.
`T_max` bounds how long our sender runs; it never enables a resubmission on its
own.

The ambiguity policy, within the paths this trigger allows, is Unresolved
(**P2-X15**).

### 6.5 Provider capability record

`llm_provider_capabilities` is immutable and versioned. Approval of a record
is an audited command by a principal. Every record is keyed by
`(provider, endpoint, api_version, streaming_mode)` and holds:

| Field group | Fields |
|---|---|
| Idempotency keys | `key_scope`, `key_max_len`; `replay_after_completion` ∈ {`returns_original`, `reexecutes`, `undocumented`}; `concurrent_in_flight` ∈ {`rejects_without_execution(<code>)`, `waits_returns_original`, `reexecutes`, `undocumented`}; `payload_mismatch` ∈ {`rejects(<code>)`, `undocumented`}; `window_seconds`; `window_start` ∈ {`first_receipt`, `completion`, `undocumented`}; `cached_outcomes`; `replay_charge`; `replay_indicator` |
| Status lookup | `status_lookup` ∈ {`by_key`, `by_provider_id`, `none`}; `record_on_acceptance`; `lookup_consistency`; `terminal_statuses`; `not_found_semantics`; `arrival_cutoff` |
| Cancellation | `cancellation_semantics` |
| Evidence | documentation source URL, retrieval date, documentation sha256, `verified_by`, `verified_at`, `audit_event_id` |
| Integrity | `contradicted` (set when observed behaviour contradicts the record) |

**Same-key retry (`idempotent_retry`) is allowed only when** an active,
non-contradicted record exists for the exact tuple; `replay_after_completion =
returns_original`; `concurrent_in_flight` is `rejects_without_execution` or
`waits_returns_original`; `payload_mismatch = rejects`; the window is known
and the current time is before `marker_time + W − margin`; the key is
unchanged; and `request_sha256` is unchanged. Any `undocumented` value
disables it.

Post-completion replay does not imply safe concurrent deduplication. A
provider "in progress" response is recorded as `provider_received` (the
provider has the request; execution undetermined).

**Same-key concurrent retry (LL11) is disabled by default** until a provider
is verified (**P2-X2**) and its capability record is approved.

### 6.6 Evidence and operator resolution

**Evidence taxonomy:**

| Evidence | Proves | Can it permit `n+1`? |
|---|---|---|
| **E-absent**: the request does not (yet) appear in provider logs, billing, console or lookup | Nothing about execution | **Never** |
| **E-rejected**: a provider record bound to this submission (our key or saved provider id) with a status documented as terminal and non-executing | Non-execution, if the provider documents the status and the key or id cannot be accepted again | Yes, if documented (P2-X2) |
| **E-terminal**: a documented guarantee that the request can no longer execute, plus proof the condition was met | Non-execution, if documented | Yes, if documented (P2-X2) |
| **E-executed**: a response, billing line or completed log entry bound to the submission | Execution | No; resolve as `executed` |

**Status lookups.** Strong consistency does not mean every request is
recorded on arrival. "Not found" proves nothing while a request may still be
travelling, may be received but not yet indexed, or may be processing. A
lookup establishes `not_executed` only when either:

- it finds the request with a status documented as terminal and
  non-executing; or
- it returns "not found", **and** the provider documents G1 (recorded on
  acceptance, before execution), G2 (read-after-write lookup) and G4 (an
  arrival cutoff after which the key cannot be accepted), **and** the lookup
  ran after the cutoff, allowing for clock skew.

G3 (terminal-state semantics) and G5 (key scope and uniqueness) must also be
documented. Any other "not found" is recorded as `still_ambiguous`.

**Minimum requirements before `not_executed` can permit `n+1`:** E-rejected
or E-terminal evidence; bound to this submission's key or saved provider id;
stored as a blob with sha256; the matching guarantee documented in a
non-contradicted capability record; the submission's spool slot sealed
(section 6.8); and the resolution requirements below. An operator's statement
alone is never sufficient.

**Without authoritative evidence**, the only permitted outcomes are
`needs_operator` (stays open) or **`abandoned_unknown`** (terminal): the call
is closed permanently, the budget is charged at presumed cost, no new
submission of the call is ever created, and any late response is recorded
only as conflict evidence and is never consumed. Whether a replacement call
may then be made is Unresolved (**P2-X23**).

**Operator resolution:**

- **Authentication:** an API command,
  `POST /v1/llm-calls/{id}/submissions/{seq}:resolve`, authenticated as a
  **user** principal (not a service account) with a recent session or key.
  `studioctl` may only be an HTTP client of this endpoint, reading credentials
  from stdin or the environment. A direct-database `studioctl` command acting
  as `system:cli` cannot carry principal authority, and an operating-system
  username proves nothing.
- **Authorisation:** a new permission, `resolve_llm_calls`, denied by default.
  `not_executed` requires two distinct principals (proposer and approver). The
  proposal is its own audited command, recorded in
  `llm_call_resolution_proposals`; the approval is a second command, by a
  different principal, that writes the resolution (C6-8). An open proposal
  blocks the promotion of late evidence (section 6.8).
- **Preconditions:** the submission is `needs_operator`; the slot is sealed; a
  `spool_scan` check exists that is no older than a configured bound, written
  by the worker's `llm.seal_and_scan` job, which seals and scans the slot (the
  API process never reads the spool; C6-9); a verified publication allows only
  `executed`; no unexplained quarantined or late evidence exists for a
  `not_executed` resolution; no other submission for the call is pending.
- **Evidence:** `evidence_type`, provider reference, `retrieved_at`, and an
  evidence blob with sha256, checked for secrets. `abandoned_unknown` needs a
  written reason.
- **Audit:** one succeeded `llm_call.resolution_proposed` event per proposal
  and one `llm_call.resolved` event per approval; a resolution that needs only
  one principal writes only the second (C6-8). Denied attempts go through the
  aggregator. `llm_call_resolutions.audit_event_id` is NOT NULL.
- **Irreversible:** resolution rows are immutable, one per submission.
  Contradicting evidence is recorded as an append-only `resolution_conflict`
  with an alert, never as an edit.

Which roles hold `resolve_llm_calls`, the session-age limit, whether four-eyes
applies beyond `not_executed`, and the accepted evidence types are Unresolved
(**P2-X22**).

**Preventing a later execution from undermining a retry.** Each call is
consumed at most once. A submission whose state is not `responded` is never
consumed. After a call is resolved, abandoned or consumed, the trigger forbids
moving any of its submissions to `responded`; late results become
`late_evidence_conflict`. Late evidence that contradicts an E-rejected or
E-terminal resolution creates a `resolution_conflict`, raises an alert
(section 3.3), and sets `contradicted` on the capability record, which blocks further
`not_executed` and `idempotent_retry` decisions relying on it until it is
re-approved. Periodic billing or usage reconciliation is Unresolved
(**P2-X24**).

### 6.7 Spool

- **Location:** a dedicated `spool/` directory on the same filesystem as the
  blob root. The spool is staging only; final bodies are promoted into Phase 1
  M5 blobs through the M5 promotion protocol.
- **Epoch:** `spool/EPOCH` holds a random id, also recorded in the database.
  If the file is missing or the ids differ (host loss, a restore, a wiped
  volume), every unresolved reservation is ambiguous. A missing marker is
  trusted only when the epoch matches.
- **Per submission:** at reservation, the slot `spool/<call>/<n>/slot/` is
  created. The writer creates `slot/pub.tmp.<writer_id>/` containing
  `response.body` and `meta.json` (call id, seq, request sha256, spool epoch,
  provider id, model, body length, body sha256, provider completion marker,
  receipt time, `received_after_lock_loss`), fsyncs both files and the
  directory, renames `pub.tmp.<writer_id>` to `pub` with a **no-replace
  rename** (`renameat2` with `RENAME_NOREPLACE` on Linux), and fsyncs the
  parent. A plain `rename(2)` silently replaces an existing empty directory, so
  it is never used for publication, sealing or late publication (C6-3).
- **Verification during recovery.** A publication is accepted only if `pub/`
  exists; `meta.json` parses; call id, seq, request sha256 and epoch match the
  database; body length and recomputed sha256 match `meta`; the provider
  completion marker is present; and the body parses under the provider's
  response schema. On any failure the verdict is recorded in the database as a
  failed `spool_verification` check, the failing directory is **copied** to
  `quarantine/`, and the submission stays ambiguous. Nothing is ever moved out
  of `sealed/` (C6-3).
- **Quarantine** means only "failed verification". It is never read as
  non-publication or non-execution, and nothing is consumed from it.
  Quarantined copies are never removed automatically; how long they are kept
  is Unresolved (**P2-X27**).
- **Cleanup (C6-2).** A spool sweep deletes only:
  - `pub.tmp.*` debris and incomplete `late/` staging directories, once past
    the cleanup threshold (section 6.10); and
  - a verified `pub` or `late/` entry, only after its body is an M5 blob
    **and** a committed database row references that blob's sha256
    (`responded`, `responded(late)`, `superseded_response`,
    `late_evidence_conflict` or `resolution_conflict`).

  The sweep takes the call row lock and re-checks the database before each
  delete, and writes one audit event per sweep, mirroring
  `blob.pending_swept` (Phase 1 section 8.1). Because backups exclude the
  spool, promoting evidence to blobs before any delete is also what lets it
  survive a backup.
- **Backups** exclude the spool. After a restore, the epoch differs and
  outcomes stay conservative.

### 6.8 Seal fence and late evidence

- Before classifying an unresolved submission, a reconciler holding the row
  lock, and whose advisory try-lock succeeded (section 6.2, C6-7), **seals**
  the slot by renaming `slot/` to `sealed/` with a no-replace rename.
- At seal time it records a **seal manifest** in the database: the name, size
  and sha256 of every entry in `sealed/` except `pub.tmp.*` (C6-3).
- **The seal is one-shot.** `sealed/` is never renamed, replaced, deleted or
  written by the system afterwards, except that the sweep removes it under
  the rules of section 6.7. A second seal attempt fails because `slot/` no
  longer exists, and changes nothing.
- Renames are atomic, so either the writer's publication happened before the
  seal (and the reconciler finds `pub` inside `sealed/`), or the writer's
  rename fails because the path no longer exists.
- A writer whose rename fails publishes its response to
  `late/<call>/<n>/<writer_id>/` with the same protocol as `pub`: a temporary
  directory, fsync of the files and the directory, a no-replace rename, and an
  fsync of the parent (C6-11). `late/` is an append-only evidence area,
  verified like `pub`. **Nothing is ever consumed directly from `late/`**
  (C6-1).
- After sealing, the slot cannot receive a normal publication. That proves
  nothing about execution or about whether a late response exists.
- A re-scan whose result differs from the seal manifest raises a
  `seal_violation` alert (section 3.3).

**Promotion of late evidence (C6-1).** A reconciler may promote exactly one
verified late entry to `responded(late)` only when all of the following hold,
while it holds the call row lock and the submission row lock:

- the submission is `ambiguous` or `needs_operator`;
- there is no resolution row and **no open resolution proposal**;
- no submission `n+1` exists;
- there is no consumption row for the call;
- the call is not `abandoned_unknown`;
- there is exactly one verified late entry for `(call, n)`;
- its body has been promoted to an M5 blob, and a passing
  `spool_verification` check references that blob's sha256.

Consumption then follows through the normal consume job (section 6.11). In
every other case the outcome is `late_evidence_conflict` and the evidence is
kept.

| State of the call | Outcome for verified late evidence |
|---|---|
| Every promotion condition above holds | `ambiguous` or `needs_operator` → `responded(late)`; consumed at most once, by the consume job |
| Anything else, including two late entries for one submission | `late_evidence_conflict` (with an alert when there are two entries); never consumed |

**Authoritative state** is the database submission row and the consumption
row. The spool is evidence only. After losing its lock, a sender never writes
to the database. Reconcilers run in River-fenced transactions, take
`SELECT … FOR UPDATE` on the submission row, and use compare-and-set
(`UPDATE … WHERE state = $expected`).

**Submission state machine (C6-6).** This is the complete set. A trigger
enforces the transitions; every transition not listed is rejected, and the
terminal states have none.

| State | Terminal | Meaning |
|---|---|---|
| `reserved` | no | Reserved and audited; not yet known to be sent |
| `ambiguous` | no | An `intent` marker exists, with no verified publication and no recorded `not_sent` |
| `needs_operator` | no | Ambiguous, and no trigger path of section 6.4 applies |
| `responded` | no | A verified response is recorded; awaiting consumption |
| `not_sent` | yes | Rejected before any byte was written |
| `failed_not_executed` | yes | An error documented as non-executing |
| `failed_charged` | yes | An error that may have been charged |
| `resolved_executed` | yes | Established as executed (lookup or operator) |
| `resolved_not_executed` | yes | Established as not executed under section 6.6 |
| `abandoned_unknown` | yes | Closed without authoritative evidence (section 6.6) |
| `superseded_response` | yes | Responded, but another submission of the call was consumed first (section 6.11) |

| From | Allowed to | By |
|---|---|---|
| `reserved` | `responded`, `not_sent`, `failed_not_executed`, `failed_charged`, `ambiguous` | the send job's record step, or the reconciler |
| `ambiguous` | `needs_operator` | the reconciler |
| `ambiguous`, `needs_operator` | `responded` (late) | the reconciler, only under the promotion rule above |
| `ambiguous` | `resolved_executed`, `resolved_not_executed` | the reconciler, only through a lookup that satisfies section 6.6 (LL12) |
| `needs_operator` | `resolved_executed`, `resolved_not_executed` | such a lookup, or operator resolution (section 6.6) |
| `needs_operator` | `abandoned_unknown` | operator resolution only (section 6.6) |
| `responded` | `superseded_response` | the consume job (section 6.11) |

`provider_received` and `still_ambiguous` are **lookup outcomes**, recorded as
checks; they are not states. `late_evidence_conflict` and
`resolution_conflict` are **append-only conflict records** with an alert; they
never change a submission's state. `idempotent_retry` is a creation path for
`n+1` (section 6.4).

A move to `responded` must reference a passing `spool_verification` check and
a promoted blob with a matching sha256.

### 6.9 Filesystem qualification (Assumption; validation mandatory before N2)

The seal fence and spool durability rest on these **Assumptions**. They are
not guarantees.

- **Single local POSIX filesystem.** The spool, its slots and the blob root
  are on one local filesystem. Network filesystems are out of scope.
- **Path-based fencing only.** Writers operate on full paths. They must not
  retain directory file descriptors, or any alternate reference (hard link,
  bind mount, open handle to a slot directory), that would bypass the renamed
  path.
- **The original slot path is never recreated.** Only reservation creates a
  slot. A writer that finds its slot missing writes to `late/` and never
  creates `slot/`.
- **Atomic rename and fsync durability are separate requirements.** Rename
  atomicity gives the ordering that makes the seal a fence. Fsync durability
  (of files and of their parent directories) gives crash survival. Each is
  validated on its own; neither implies the other.
- **No-replace rename is required.** The filesystem must support
  `renameat2(RENAME_NOREPLACE)`, because a plain rename can replace an empty
  `sealed/` or `pub/` (C6-3). Start-up refuses a filesystem without it.
- **Filesystem-specific fault-injection validation is mandatory before N2
  implementation.** It must be performed on the target filesystem, and the
  N2 milestone authorisation must fix its method. Proposed tests are FS1–FS7
  (section 6.15). The target filesystem is Unresolved (**P2-X31**; the
  recommended default is the development host's local ext4, because
  deployment is excluded).

### 6.10 `D_pub` is a cleanup threshold only

`D_pub = marker_time + T_max + T_pub + margin` marks only when leftover
`pub.tmp.*` debris and incomplete `late/` staging directories become eligible
for sweeping. It never makes a verified `pub` or `late/` entry, or a
quarantined copy, eligible (C6-2, section 6.7). Process suspension,
filesystem stalls, or a pause between fsync and rename can delay a
publication for any length of time. `D_pub` is **never** used in a decision.
Decisions rely on the seal (section 6.8) and on evidence (section 6.6).

### 6.11 Consumption

`llm_call_consumptions` is unique on `call_id`. The consume job's fenced
transaction locks the call row and requires a `responded` submission. If two
submissions of a call both reach `responded` before either is consumed, the
first committed is consumed and the other becomes `superseded_response`,
recording a presumed duplicate charge. This can happen only on the same-key
retry path (section 6.5). A late response for `n` that arrives after `n+1`
exists never becomes `responded`; it is a `late_evidence_conflict` (C6-5).
Consumption is exactly once because the database enforces it. Execution at the
provider is not.

### 6.12 Audit (Phase 1 section 8 applies unchanged)

**Phase 1 section 8.1 is not changed.** Every committed change to ledger or
domain state is a transaction with its own single audited event. River
bookkeeping is already excluded by Phase 1 section 8.1. Spool files are
filesystem staging, not database commits.

| When | What is recorded | Where | Audit |
|---|---|---|---|
| Before submission | Call intent; reservation; historical rights decisions | `llm_calls`, `llm_call_submissions`, `rights_decisions` | One event for the reserving step (batch form), with `enqueued_jobs` |
| At fetch | Job attempt | `river_job` | River bookkeeping (excluded by Phase 1 section 8.1) |
| After submission | `intent` marker, provider id, response | Spool | Filesystem staging; sweep writes one event per sweep |
| After the response | Outcomes, usage, actual or presumed cost, egress summary | `llm_call_submissions` | One `job_execution` event for the send job |
| Domain commit | Candidates and checks | Consume job | One event per job |
| After recovery | Ambiguous outcomes and how each was resolved; abandoned calls; late promotions and conflict records | `llm_call_submissions`, reconcile job | The recovering job's event, or a periodic event (no causation, per JA6) |
| Seal and scan | Seal manifest; `spool_scan` and `spool_verification` checks | `llm.seal_and_scan` job | One `job_execution` event per job |
| Operator resolution | Proposal; resolution, evidence reference, proposer and approver | `llm_call_resolution_proposals`, `llm_call_resolutions` | One `llm_call.resolution_proposed` event per proposal and one `llm_call.resolved` event per approval (C6-8) |

Causation chains (Phase 1 section 8.2) link the run request to the reserve,
send and consume events.

### 6.13 Guarantees

**Achievable:** no send without a committed, audited reservation; a verified,
published response is never paid for again; a call is never resent under the
same `seq`; each call is consumed at most once; each call's history can be
recovered on its own; every attempt that may have been charged is accounted
for at its actual or presumed cost; spending stays bounded by reserved
ceilings; runs can be replayed from stored responses.

**Not achievable without provider support:** exactly-once execution or
charging; resolving an ambiguous send; identical output on resubmission;
control over provider data retention beyond its contract.

**Never claimed:** that the advisory lock guarantees exclusive execution; that
cancellation proves non-execution; that `D_pub` or quarantine proves
non-publication or non-execution; that "not found" proves non-execution
without the guarantees in section 6.6; that a seal proves anything about
execution or about late responses.

### 6.14 Budget

Each reservation records a budget ceiling. The budget check at each attempt
counts the presumed cost of every unresolved submission. Overspend is limited
to the in-flight ceilings.

### 6.15 Proposed tests (LL and FS)

**None of these tests is implemented or has been run.** They are proposed
acceptance tests for N2. LL42 to LL52 and FS7, and the extended expectations
of LL25d, LL25f and LL41, were added by correction C6-4 (2026-11-07). Fault
injection uses an in-test TCP proxy between worker and PostgreSQL, a fake provider (execution counter, delays, lookup
store, indexing delay, arrival cutoff, capability modes, and the ability to
break its own documented guarantee), a spool filesystem that can inject
faults, and pause hooks passed in as dependencies (no package-level state).

| ID | Scenario | Expected result |
|---|---|---|
| LL1 | Crash before the reservation commits | No submission row, no marker; a retry reserves |
| LL2 | Crash after the reservation, before the marker | The retry sends exactly once |
| LL3 | Crash after the marker, before any HTTP byte | Ambiguous (conservative); without idempotency, `needs_operator` |
| LL4 | Crash during streaming | Partial body; ambiguous; never consumed |
| LL5 | Crash after publication, before the database record | Recovery verifies the spool and records `responded`; no resend |
| LL6 | Published body hash mismatch | Quarantined; ambiguous; not consumed |
| LL7 | `meta` mismatch (call, seq, request hash or epoch) | Quarantined; ambiguous |
| LL8 | Completion marker missing, or body fails to parse | Quarantined; ambiguous |
| LL9 | Lock connection cut during an active request | Watchdog detects within *w*; request cancelled; nothing further sent; job returns an error |
| LL10 | Another worker takes the lock while the first request is in flight; no idempotency or lookup support | No send; `n+1` rejected; `needs_operator`; at most one execution |
| LL11 | As LL10 with an approved capability record documenting safe concurrent deduplication | `n+1` with the same key; one execution in total. **Disabled by default until P2-X2.** |
| LL12 | As LL10 with a lookup satisfying section 6.6 and a saved provider id | Outcome resolved by lookup; no resend if executed |
| LL13 | Response arrives after lock loss | Published or written to `late/`; consumed at most once |
| LL14 | Raw-SQL insert of `n+1` while `n` is unresolved, provider not idempotent | Rejected |
| LL15 | Raw-SQL insert of `n+1` after the idempotency window | Rejected |
| LL16 | Database commit by a sender after another River attempt took over | Fence rejects; error returned |
| LL17 | Partial batch: some published, one marker only, some not reached | Published ones recorded from spool; marker-only one ambiguous; unreached ones sent once each |
| LL18 | Spool epoch mismatch | Every unresolved submission ambiguous; nothing resent automatically |
| LL19 | Writes never fsynced are dropped | No `pub/` without a complete fsync sequence; recovery conservative |
| LL20 | Request reaches `T_max` | Cancelled; ambiguous; no automatic `n+1` |
| LL21 | Operator resolutions `executed`, `not_executed` (with evidence), `abandoned_unknown` | Each writes one audited event; only `not_executed` allows `n+1`; `abandoned_unknown` closes the call |
| LL22 | Ambiguous submission | Counted against the budget at presumed cost |
| LL23 | Second consumption of a call | Rejected by the unique constraint |
| LL24 | Server terminates the lock session without a network fault | As LL9 |
| LL25 | First worker loses its lock mid-request; second worker takes the lock and seals; the response then arrives | First worker's rename fails; response in `late/`; second worker records ambiguous; next reconciliation records `responded(late)`; consumed once; one execution |
| LL25b | Publication completes before the seal | Reconciler finds `pub` in `sealed/`; records `responded` |
| LL25c | Writer paused between writing the body and renaming; seal happens | Writer's rename fails; response in `late/`; temporary directory ignored |
| LL25d | Sweep at the cleanup threshold races a paused writer | Nothing lost; nothing consumed from quarantine; the writer's late entry survives every sweep until it is promoted to a blob and referenced (C6-2) |
| LL25e | `not_executed` attempted before the slot is sealed | Rejected |
| LL25f | `n+1` responds, then a late response for `n` arrives | `late_evidence_conflict` for `n`; one consumption (of `n+1`); `n` never becomes `responded` (C6-5) |
| LL25g | Late response after a resolution | `late_evidence_conflict`; alert; resolution unchanged |
| LL26 | Capability record has `concurrent_in_flight = undocumented` | `idempotent_retry` rejected; `needs_operator` |
| LL27 | Same key, different `request_sha256` | Rejected |
| LL28 | Window bound reached | `idempotent_retry` rejected |
| LL29 | Provider returns "in progress" to a concurrent retry | `provider_received`; no duplicate execution |
| LL30 | Lookup "not found" from an eventually consistent provider | Not treated as `not_executed` |
| LL31 | Writing or fsyncing `lock_lost` fails | Request still cancelled; error returned; classification still ambiguous |
| LL32 | Resolution by a service account, by one principal for `not_executed`, without evidence, or via `studioctl` as `system:cli` | Each rejected with a denied event |
| LL33 | Second resolution for a submission, or UPDATE to a resolution row | Rejected |
| LL34 | `not_executed` with only E-absent evidence | Rejected |
| LL35 | Valid E-rejected resolution, `n+1` runs, then the fake provider breaks its guarantee and runs the original | `resolution_conflict`; alert; capability `contradicted`; one consumption; further retries using that capability rejected |
| LL36 | No authoritative evidence | Only `needs_operator` or `abandoned_unknown`; afterwards no consumption and `n+1` rejected |
| LL37 | Strongly consistent "not found" while the request is held in transit (no G4) | `still_ambiguous`; `n+1` rejected; later execution recorded as late evidence |
| LL37b | As LL37 with G1, G2, G4 configured and the lookup after the cutoff | `not_executed` allowed; late arrival rejected by the fake provider; zero executions |
| LL37c | Received but not yet indexed (asynchronous indexing, no G1) | `still_ambiguous` |
| LL38 | Writer paused between fsync and rename until after `D_pub`; seal; writer resumes | Rename fails; response in `late/`; `responded(late)` only if the call is unresolved; consumed once |
| LL38b | Writer paused after the response arrives, before creating its temporary directory; seal | Writer cannot create the slot; writes to `late/` |
| LL38c | 1,000 concurrent iterations of writer rename versus seal | Each time either `pub` is in `sealed/` or the writer lands in `late/`; never both missing |
| LL38d | Something writes into `sealed/` after the seal | `seal_violation` alert |
| LL39 | Publication fails verification and is quarantined; writer then attempts a late publish | Written to `late/`; state stays ambiguous; quarantine not read as non-publication |
| LL40 | Late response after a valid `not_executed` and `n+1` | `late_evidence_conflict`; alert; never consumed; capability `contradicted` |
| LL41 | `D_pub` passes with no seal and no evidence | No state change; no evidence file deleted |
| LL42 | Late promotion attempted in each disallowed state: a resolution, an open proposal, `n+1` exists, already consumed, `abandoned_unknown` | `late_evidence_conflict` each time; never consumed (C6-1) |
| LL43 | Sweep against a verified late entry that is not yet promoted | Nothing deleted; after promotion and the referencing row, the entry is deleted (C6-2) |
| LL44 | Sweep with quarantined copies present | Quarantine untouched (C6-2) |
| LL45 | Crash between promoting a late body to a blob and the database commit | Re-promotion is idempotent; one consumption |
| LL46 | Two verified late entries for the same `(call, n)` | Conflict and alert; neither consumed |
| LL47 | An empty `sealed/` or `pub/` exists before the seal or publication | The no-replace rename fails; nothing is overwritten (C6-3) |
| LL48 | A second seal attempt on an already sealed slot | Fails; no state change |
| LL49 | Contents of `sealed/` differ from the seal manifest on re-scan | `seal_violation` alert (C6-3) |
| LL50 | Lease expired while a live sender still holds the advisory lock | The reconciler skips the call; nothing sealed; no ambiguity created (C6-7) |
| LL51 | A resolution proposal races a late promotion | Exactly one wins under the row locks; the other is rejected or recorded as a conflict (C6-1, C6-8) |
| LL52 | A late entry whose `meta` epoch differs from the database | Copied to quarantine; never promoted |

**Filesystem validation (FS, mandatory before N2 implementation; method fixed
in the N2 milestone authorisation):**

| ID | Validates |
|---|---|
| FS1 | Rename atomicity on the target filesystem, for files and for directories (including rename onto an existing non-empty directory failing) |
| FS2 | After a slot is renamed, a writer's path-based rename into it fails, under concurrency |
| FS3 | Fsync durability of files and their parent directories across a simulated power loss, tested separately from FS1 |
| FS4 | `O_CREAT|O_EXCL` exclusivity |
| FS5 | The spool and blob root are on the same filesystem (device check at startup) |
| FS6 | Startup refuses unsupported filesystems, including network filesystems |
| FS7 | `renameat2(RENAME_NOREPLACE)` is supported on the target filesystem and refuses an existing target, including an empty directory (C6-3) |

---

## 7. Rights and data governance (Approved direction, R3-AP5)

### 7.1 Purposes and use context

**Purposes**, each evaluated independently, with no implication between them:
`store`, `parse`, `derive`, `llm_local`, `llm_external`, `train`, `evaluate`,
`include_text`.

- `parse` means **creating or reading the parsed representation**.
- `commercial` is not a purpose. It is a **use context** `c ∈
  {non_commercial, commercial}`. Grants permit `(purpose, contexts)` pairs. A
  release declares its `intended_purposes` and `use_context`. The context for
  processing inside the installation is Unresolved (**P2-X16**, L6).

### 7.2 Rights statements and evaluation

Statements are append-only (Phase 1 section 5.6) and record: source; kind
(`grant`, `restriction`, `withdrawal`, `correction`); permitted and denied
purposes with contexts; obligations (`attribution`, `share_alike`, `notice`,
`other`); unmapped conditions (free text); effective interval;
`recorded_at` (set under the `RIGHTS_EPOCH` lock); `supersedes`.

Phase 1 statements carry no use context. In Phase 2 evaluation, a statement
with no context matches no context, so it yields **unknown**, which is deny.
Phase 1's `rights.Evaluate` is unchanged, and the Phase 2 columns are added,
never altered (X-C).

Evaluation is bitemporal: `Eval(s, p, c, t_eff, t_known)`.

1. An applicable `withdrawal` denies every purpose except `store` (retention
   of bytes is Unresolved, P2-X9).
2. An applicable statement that denies `p` in context `c` gives **deny**.
3. An applicable statement that permits `p` in `c`, with no denial and no
   unmapped condition covering `p`, gives **allow** with the union of
   obligations.
4. Otherwise **unknown**, which is treated as **deny**. Free-text conditions
   are never interpreted.

### 7.3 Derivation closure

`Src(X)` is the set of sources reached through X's evidence anchors; the
sources of each premise revision, recursively; **every content unit sent as
LLM input** in the calls that produced X, whether or not it is cited; for
output records, all pinned knowledge and gap revisions and the transform's LLM
inputs; and any source content in prompt templates (Unresolved, P2-X20).

`Decision(X, p, c, t)` is allow only if every source in `Src(X)` evaluates to
allow. Obligations are the union.

### 7.4 Operations and required purposes

Every operation requires only the purposes for what it actually does,
evaluated for every source in the closure of its inputs, as a **current**
allow at the start of the operation. In the table, "+ `parse`" applies only
when the operation reads source content (content units or original artifact
bytes).

| Operation | Purposes required |
|---|---|
| Keep an artifact | `store` |
| Parse an artifact into content units | `parse` |
| Read parsed content units | `parse` |
| Read original artifact bytes (any representation, even temporary) | `store`, `parse` |
| Deterministic derivation producing knowledge, records or gaps | `derive` (+ `parse`) |
| Local LLM producing derived content | `llm_local`, `derive` (+ `parse`) |
| External LLM producing derived content | `llm_external`, `derive` (+ `parse`) |
| Local or external LLM check producing only check records | `llm_local` or `llm_external` (+ `parse`) |
| Deterministic check producing only check records | `parse` if it reads source content; otherwise none |
| Read check records with `contains_source_content = false` | None (project authorisation only) |
| Read check records with `contains_source_content = true` | `parse` |
| Freeze a release: members with `record_derivations` rows | `derive`, plus each intended purpose in the declared `use_context`; `include_text` if any member contains verbatim source text or check records with source content |
| Freeze a release: Phase 1 members with no derivation | The Phase 1 freeze gates only, unchanged (X-B) |

Check records with no source content stay readable after a source is
withdrawn, for audit.

**`store` and the ingestion order (X-A).** Phase 1 uploads an artifact before
any rights statement exists (Phase 1 sections 5.6 and 11), while unknown means
deny here. Phase 1 upload is unchanged. How `store` is then applied is
**Unresolved (P2-X25)**. The recommended default, not decided: `store` is
evaluated by Phase 2 operations and by `rights.review`; an artifact whose
`store` is not allow is flagged and never processed; whether it is deleted
follows P2-X9.

### 7.5 Historical and current decisions

- **Historical:** each step writes append-only `rights_decisions` rows (step,
  source, purpose, context, time, outcome, statement ids). A step proceeds only
  if all are allow.
- **Current:** freeze evaluates `Decision(member, p, c, rights_as_of)` for
  every member and purpose. An earlier allow never carries over to a new
  release.
- **Processing provenance:** freeze also requires a recorded historical allow
  for every step in each member's production closure.
- **Retroactive corrections:** if a statement recorded after a step changes
  the answer for that step's time, the item is flagged `retroactive_conflict`
  and freeze is blocked until a decision is made (**P2-X19**).

### 7.6 Withdrawal, revocation and released versions

- Withdrawal immediately blocks new processing; knowledge depending on the
  source is computed as rights-blocked; drafts containing it cannot be frozen.
- Released versions stay immutable, with their manifest's rights snapshot.
- A periodic `rights.review` job re-evaluates released versions, writing
  append-only `release_rights_reviews` rows and an audit event, with outcome
  `still_allowed` or `now_denied`.
- `now_denied` requires a human decision: retract (the existing Phase 1
  operation) or keep with a recorded justification. Whether downloads of a
  `now_denied` release are blocked automatically is Unresolved (**P2-X18**).

### 7.7 Data governance requirements (GOV)

| # | Requirement |
|---|---|
| GOV-1 | No processing beyond storage without recorded terms. Unknown means deny. |
| GOV-2 | Every pipeline step declares its purposes and evaluates them at its start; a denial fails the step and is audited. |
| GOV-3 | External LLM calls go only to approved, capability-recorded providers. Each call requires `llm_external` for every source whose content is sent. Only the needed content units are sent. The ledger stores request and response hashes and blobs. Content never appears in logs. A configuration kill switch disables all egress. Egress is audited per step. |
| GOV-4 | Rights propagate along the derivation closure (section 7.3). |
| GOV-5 | Verbatim source text needs `include_text`, gated separately (P2-X10, L4). |
| GOV-6 | Attribution obligations are listed in the manifest. |
| GOV-7 | Sources can be withdrawn; byte retention is Unresolved (P2-X9). |
| GOV-8 | No credentials, repository content or unrelated data is ever sent to a provider. Source content is untrusted: models get no tools, and output is schema-validated. |
| GOV-9 | Legal review before any real data (Phase 1 Assumption, unchanged). Real data is never used in automated tests. |
| GOV-10 | Sources may contain personal data, for example patient details in case reports (X-K). The policy is **Unresolved (P2-X26, L12)**. The recommended default, not decided: exclude such sources, flagged in source metadata, until a data-protection review. |

### 7.8 Questions for legal review (no conclusions drawn)

| # | Question |
|---|---|
| L1 | Are knowledge units, which restate facts and claims, derivative works under each target licence? |
| L2 | May a source be sent to an external LLM provider under each licence and jurisdiction? |
| L3 | Is model training or evaluation permitted under each licence, and do text-and-data-mining exceptions apply? |
| L4 | When may verbatim excerpts be included in a dataset, and in what form? |
| L5 | Do paraphrased or generated scenarios count as adaptations? |
| L6 | How do non-commercial terms apply to training, datasets and in-house processing? |
| L7 | Does share-alike attach to derived datasets or to trained models? |
| L8 | What attribution format satisfies obligations inside datasets? |
| L9 | Can a licence be revoked, and what follows for releases already distributed? |
| L10 | Do database rights (such as the EU sui generis right) apply? |
| L11 | How long may source copies be retained, and what does a takedown require? |
| L12 | What legal basis, if any, permits processing personal data that appears in sources, and what does it require (X-K)? |

---

## 8. Audit summary (Approved direction)

Phase 1 section 8 applies to all Phase 2 code with **no exceptions**. Every
command writes exactly one succeeded event in its transaction; every job
writes one `job_execution` event; batches use the Phase 1 batch form;
identity decisions and operator resolutions are linked to their events
(sections 5.5 and 6.6). No Phase 2 table is exempt from audit. Bulk exports
of content units and knowledge are audited as data egress (section 4).

---

## 9. Extraction and verification (Proposed)

- **Deterministic inventory** per content unit: sentence boundaries, numeric
  tokens with units and context, table cells, hedge and negation cues
  (lexicon held as vocabulary data), comparatives, citation markers,
  cross-references.
- **Proposition enumeration:** a separate LLM pass that lists atomic
  propositions per paragraph or table.
- **Structured extraction:** schema-constrained output per knowledge type;
  every unit cites spans with verbatim quotes. A provider's schema mode is used
  only where its capability record documents it, and output is always
  re-validated locally. Requests are non-streaming at first.
- **Deterministic verification:** quote match, numeric fidelity (value, unit,
  sign, precision; table header paths), qualifier diff, attribution check.
- **LLM verification** with a different prompt, ideally a different model
  family (P2-X2): entailment, qualifier preservation, relation correctness.
- **Omission detection:** unmatched propositions, uncovered sentences,
  uncovered substantive numbers and table cells, cross-references with no
  relation; targeted re-extraction until convergence or budget.
- **Within-source contradiction scan.**
- **Metrics are proxies** unless measured against human gold data. The system
  never claims an extraction is complete.

## 10. Knowledge-gap discovery (Proposed)

Categories (definitions corrected, X-L):

| Category | Meaning |
|---|---|
| `extraction_omission` | The source states it, but the knowledge base lacks it. A system defect. |
| `absent_from_source` | An aspect on the source type's checklist that the source does not address at all. |
| `unanswerable_question` | A specific generated question whose answer cannot be determined from the source, although the aspect is addressed. |
| `ambiguous` | The source supports more than one reading. |
| `inconsistent` | The source contradicts itself. |
| `insufficient_detail` | The source addresses it, but without the detail needed (for example a value without a unit). |

- `extraction_omission` is a system defect. It goes to re-extraction and is
  **never** included in a dataset.
- The loop: generate questions from the knowledge base and a per-source-type
  aspect checklist (registry content); answer from the knowledge base only;
  if unanswerable, search the content units (PostgreSQL full-text search, then
  an LLM reading the top units; no vector database); found means
  `extraction_omission`, not found means `absent_from_source` or
  `unanswerable_question`; repeat until the omission rate converges.
- Ambiguity and inconsistency come from verification and the contradiction
  scan.
- Tables: `gaps`, `gap_revisions`, `gap_links`. Deduplication by normalised
  statement, linked-unit overlap and an LLM equivalence signal. Priority by a
  configurable formula.

## 11. Generated and derived knowledge (Proposed)

- Scenarios and other training material are knowledge units with
  `origin = generated`, a registry knowledge type, a declared training
  purpose, and `knowledge_derivations` to every premise revision.
- Validation before use: premises accepted; rights allow the purpose across
  the closure; every factual proposition entailed by a premise or declared
  hypothetical; uncertainty carried over; answerability check; near-duplicate
  checks.
- Generation depends on verified knowledge (N5), not on transforms (N8).
- Whether `inferred` content may enter datasets is Unresolved (P2-X4).

## 12. Transforms and formats (Proposed)

- A dataset type is a `dataset_kind` and `record_schema` (Phase 1 registry).
- `transform_spec` (new registry kind): a selector over knowledge, gap and
  generated items with minimum check outcomes; a mapping, either deterministic
  (CEL) or generative (prompt template, validated as in section 11); split
  assignment by source, never by item, and stable across versions: a pinned
  per-dataset salt keeps each source in the same split in every release, so
  evaluation sources never leak into training.
- Serializers are generic, keyed by serialisation format, never by dataset
  type. Initial types and serializers are Unresolved (P2-X7).
- Output records are materialised projections pinned through
  `record_derivations`.

## 13. Quality (Proposed)

- **Invariants** (100%, never thresholded): schema validity, complete lineage,
  rights allow, exact numeric match where a numeric field is cited, labelling
  of generated content, pinned fingerprints.
- **Calibrated gates** (entailment, qualifier preservation, duplicates,
  unsupported assumptions, coverage floor): thresholds are experimental targets
  until a production standard is approved (P2-X6). Near-duplicates are found
  by shingling or MinHash in Go; no vector database.
- **Calibration:** labelled sets from gold data, planted claims and injected
  corruptions; false acceptance and false rejection measured per corruption
  type with confidence intervals; thresholds bound to the checker fingerprint,
  and voided when it changes; production re-audit sampling (P2-X5).
- **Freeze uses only precomputed checks.** No LLM runs at freeze.

## 14. Release extensions (Proposed)

Phase 1 freeze gates remain. Added gates: lineage completeness; generated
content labelled; rights across derivation closures (section 7); processing
provenance; quality-profile gates over precomputed checks. The added gates
apply only to members with `record_derivations` rows; Phase 1 members keep
exactly the Phase 1 gates (X-B). Manifest version 3
adds fingerprints (adapter, models, prompts, transforms, checkers, engines),
quality and coverage metrics labelled as proxies, attributions and evidence
classes.

## 15. Security additions (Proposed)

Phase 1 section 14 applies unchanged. Additions: prompt-injection containment
(no tools for models, schema-validated output, sources treated as untrusted);
egress controls (GOV-3); provider credentials only from the environment and
never logged; provider calls through the standard library HTTP client; the
operator-resolution controls in section 6.6; the API process never reads the
spool (C6-9).

---

## 16. Milestones (all Proposed)

### 16.1 Milestones

| # | Milestone | Scope summary |
|---|---|---|
| N0 | Filesystem qualification and alerts | FS1–FS7 on the target filesystem (P2-X31); start-up filesystem and free-space checks; `alerts` (section 3.3); spool epoch storage. No LLM code, no spool lifecycle |
| N1 | Content | Content versions and units, adapter interface, research-paper PDF adapter (born-digital), locators, re-anchoring |
| N2 | LLM gateway and governance | Section 6 and GOV-3; provider interface and fake provider; call ledger; spool; capability records; seal-and-scan job; operator resolution with proposals |
| N3 | Knowledge | Section 5 |
| N4 | Orchestration and extraction | Pipeline runs and steps (section 3.2); section 9 inventory, enumeration, extraction, deterministic verifiers |
| N5 | Verification | LLM verification, omission loop, contradictions, calibration (section 13) |
| N6 | Gaps | Section 10 |
| N7 | Generated knowledge | Section 11 |
| N8 | Transforms | Section 12 |
| N9 | Release extensions | Section 14 |
| N10 | Harder sources | OCR, complex tables, figures, supplementary structured files |

### 16.2 Dependencies

```
N0  ← M6                         (may run alongside M7)
N1  ← M5, M6, M11
N2  ← M5, M6, M7, M9, M11, N0    (extends Phase 1 M7 consistency tests to its
                                  tables and blobs; M7 added by C6-12)
N3  ← M10, N1
N4  ← N2, N3
N5  ← N4
N6  ← N5
N7  ← N5
N8  ← M12, N5                    (consumes validated N6 and N7 output when present)
N9  ← N8, M13, M7
N10 ← N1
```

**P2-X12, resolved by Godwin on 2026-11-07:** N1 starts after M14, and N0 may
run alongside M7. This sets order only. Every N-milestone, N0 included, still
needs its own merged milestone authorisation (section 0.2). Phase 1
dependencies (Phase 1 section 16.1) are unchanged.

### 16.3 Experiments (Path A, P2-X13)

- **EXP-1** (research-paper ingestion and extraction) is not authorised.
  **Deferred by Godwin on 2026-11-07:** no experiment is prepared or run, and
  no authorisation document for one is drafted. It is not required for
  Phase 1.
- All experiment code, prompts, corpus and results stay **outside this
  repository**. Nothing from an experiment is committed here except the
  documents that authorise and report it.
- EXP-1 needs its own scoped written authorisation, a separate pre-registration
  document fixing every threshold, analysis method, corpus, provider and
  budget before execution. That document does not exist yet.
- Governance principle (R3-AP7): no gate is exempted for experiment code;
  real source data is never committed; results are exploratory; meeting
  targets authorises an architecture review only, never a milestone.

---

## 17. Acceptance criteria and test families (Proposed)

**Proposed acceptance criteria:**

| # | Criterion |
|---|---|
| P2-AC1 | A research paper goes from source to release with full lineage from each output record to its content units. |
| P2-AC2 | No egress without rights (GV tests). |
| P2-AC3 | Extracted, inferred, generated, curated and imported content are distinguishable in every release. |
| P2-AC4 | The KI matrix passes. |
| P2-AC5 | The LLM lifecycle guarantees hold under fault injection (LL tests) on a filesystem that passed FS1–FS7. |
| P2-AC6 | The rights model holds (RV tests). |
| P2-AC7 | Extraction coverage is reported against the deterministic inventory (section 9) and labelled as a proxy; the system never claims completeness (X-G). |
| P2-AC8 | Every gap has exactly one category (section 10), and no `extraction_omission` is ever released (X-G). |
| P2-AC9 | Every generated item is validated (section 11), labelled `generated`, and linked to all its premise revisions (X-G). |
| P2-AC10 | At least two dataset types and two serialisation formats are produced from the same knowledge, with no dataset-type-specific code (X-G). |
| P2-AC11 | Calibrated gates are bound to checker fingerprints and voided when they change; invariants are never thresholded (X-G). |
| P2-AC12 | An end-to-end run on a synthetic corpus, through release, rebuilds byte-identically after backup and restore (X-G). |

**Test families** (all proposed; none implemented):

| Family | Covers |
|---|---|
| CU | Content: locator round-trip, re-anchoring outcomes, content-version immutability |
| KI | Knowledge identity (section 5.6) |
| LL | LLM call lifecycle (section 6.15) |
| FS | Filesystem validation (section 6.15) |
| RV | Rights evaluation: unknown means deny; deny wins; withdrawal; closure includes uncited LLM inputs; bitemporal decisions; retroactive conflicts; operation-to-purpose mapping; use context; check-record access; release rights review |
| GV | Egress governance: no egress without `llm_external`; only needed units sent; kill switch; no content in logs; every send has a reservation; only approved providers |
| QC | Calibration: thresholds bound to fingerprints; voided on change; false acceptance and false rejection per corruption type; invariants never thresholded |
| OR | Orchestration: resumable and cancellable runs; idempotent steps; per-run budgets; purposes evaluated at each step |
| EX | Extraction: quote match; numeric fidelity; qualifier diff; attribution; omission detection |
| GP | Gaps: categories; omissions never released; deduplication |
| GN | Generation: premise linkage; entailment of factual propositions; labelling; near-duplicates |
| TR | Transforms and serializers: lineage; stable source-level splits; generic serializers |
| RL | Release extensions: added freeze gates; manifest version 3; golden hashes |

None of these prefixes collides with a Phase 1 test identifier.

---

## 18. Exclusions (Proposed)

Phase 1 exclusions (Phase 1 section 18) remain fully in force for Phase 1.
Phase 2 additionally excludes, unless a later approved decision changes it:

- integration with the separate Senebiclabs expert-answering system;
- the Senebiclabs website, dataset catalogue, and external distribution of
  releases;
- HEALTH and Workforce integration;
- deployment, TLS and KMS;
- multi-tenancy;
- a web UI (pending P2-X5);
- deleting committed blobs (pending P2-X9);
- real data in automated tests (always);
- sources containing personal data, pending P2-X26 and L12;
- experiments (EXP-1 deferred, section 16.3);
- a graph database or vector database without demonstrated need.

---

## 19. Risks (Proposed)

| Risk | Mitigation |
|---|---|
| Completeness cannot be proven | Proxy metrics labelled as such; gold-data measurement; omission loop |
| Verifier correlated with extractor | Different prompt; different model family where possible (P2-X2) |
| Parser quality on tables, figures and scans | `derivation` flags; review; N10 |
| Unstable knowledge identity | Section 5 rules and KI tests |
| LLM cost and duplicate charges | Ledger, reservations, budgets, section 6 recovery rules |
| Provider guarantees change or are wrong | Capability records with documentation hashes; `contradicted` flag |
| Filesystem assumptions do not hold | Section 6.9; FS validation before N2 |
| A plain rename overwrites an empty `sealed/` or `pub/` | No-replace renames; FS7; LL47 (C6-3) |
| Verified late evidence lost to cleanup | Sweep rules of section 6.7; LL43 to LL45 (C6-2) |
| Legal status of derived content | L1–L12; GOV-9 |
| Prompt injection from sources | Section 15 |
| Model deprecation | Stored responses allow replay and byte-identical rebuilds |

---

## 20. Registers

### 20.1 Decisions (approved by Godwin, 2026-11-06 and 2026-11-07)

Decision ids use the prefix R3-AP to avoid colliding with Phase 1 test ids
D1–D7.

| # | Decision | Level |
|---|---|---|
| R3-AP1 | Phase boundary: Phase 1 unchanged at Revision 2.2 with a non-normative pointer; Phase 2 tracked in this document | Approved |
| R3-AP2 | Architecture direction: the 11 decisions in section 1 and the corrected designs | Approved direction only; no implementation authority |
| R3-AP3 | Authorisation model: standing authorisation M1–M14 only; one milestone authorisation per Phase 2 milestone; written authorisation means a merged, scoped document | Approved |
| R3-AP4 | LLM call lifecycle and audit (section 6), including the evidence taxonomy, provider capability requirements, status lookup requirements, operator-resolution model, `abandoned_unknown`, late-evidence handling, `D_pub` cleanup semantics, and LL1–LL41 as proposed tests | Approved direction, subject to the filesystem qualification (section 6.9) |
| R3-AP5 | Rights model (section 7), with `parse` defined as creating or reading the parsed representation | Approved direction |
| R3-AP6 | Knowledge identity (section 5) | Approved direction |
| R3-AP7 | Experiment governance principle (section 16.3) | Approved as a principle only |
| P2-X13 | Experiment placement and authority: **Path A** | Resolved |
| P2-X12 | When Phase 2 starts: N1 after M14; N0 alongside M7 (section 16.2) | Resolved, 2026-11-07 |
| Corrections | C6-1 to C6-12 and X-A to X-L incorporated before this document's first merge. Each keeps the label of the section it amends; the owner and legal decisions they surface are Unresolved below. | Approved for incorporation, 2026-11-07 |
| EXP-1 | Deferred; nothing prepared or run (section 16.3) | Decided, 2026-11-07 |

### 20.2 Unresolved

Each item is undecided. The last column is a recommendation recorded for the
decision session; it is **not** a decision and gives no authority to build.

| # | Item | Needed by | Recommended default (not decided) |
|---|---|---|---|
| P2-X1 | Parser tool, and acceptance of a parser not written in Go | N1; EXP-1 if resumed | A pinned non-Go tool run by the worker is acceptable; choose by licence, table fidelity, coordinates and determinism; no AGPL without an explicit decision |
| P2-X2 | Provider verification and capability records: model family for verification, data retention terms, idempotency and status-lookup guarantees | N2; EXP-1 if resumed | Verify one external provider from its documentation, plus the fake provider; require no training on inputs and documented retention; local models deferred |
| P2-X3 | Knowledge-base scope | N3 | Per project; content versions shared per source |
| P2-X4 | Whether inferred content may enter datasets | N7, N8 | No by default; opt-in per transform, labelled |
| P2-X5 | Human review: whether a web UI is added; review sampling rate | N3 (queues), N9 (sampling) | Review queues through the API and `studioctl` (section 5.5); no web UI; sampling rate set after calibration |
| P2-X6 | Production quality standards; weighting of false acceptance against false rejection | N9 | Calibrated gates stay experimental until gold data exists; favour false rejection |
| P2-X7 | Initial dataset types and serializers, including whether gaps feed evaluation items | N8 | Grounded QA for evaluation (with unanswerable items from gaps), grounded instruction pairs for training, claim verification; JSONL first |
| P2-X8 | Source languages | N4 | English first |
| P2-X9 | Takedown versus never deleting committed blobs | N9 | Never delete through N8; decide with legal (L11) before N9 |
| P2-X10 | Verbatim source text in datasets (with L4) | N8 | Excluded until legal review |
| P2-X11 | Role mapping for Phase 2 operations, including `resolve_llm_calls` | N2, N3 | Run pipelines and review actions: owner, maintainer; read knowledge: viewer and above; `resolve_llm_calls` as in P2-X22 |
| P2-X14 | Send batch size | N2 | 16, configurable |
| P2-X15 | Ambiguity policy, within the paths the trigger allows | N2 | Always `needs_operator` unless a documented path applies |
| P2-X16 | Use context for in-house processing (with L6) | N2 | `commercial`, the conservative choice |
| P2-X17 | Whether reported precision counts toward identity | N3 | No; fidelity-checked only |
| P2-X18 | Whether downloads of `now_denied` releases are blocked | N9 | Blocked automatically until the human decision |
| P2-X19 | Retroactive-correction policy | N9 | Freeze blocked until a principal decides |
| P2-X20 | Source content in prompt templates; purpose vocabulary sign-off | N2 | Forbidden, with synthetic few-shot examples only; the eight purposes approved as written |
| P2-X21 | Watchdog interval *w*, `T_max`, the lease and the cleanup threshold | N2 | *w* = 5 s; `T_max` = 300 s; lease = `T_max + 2w + 60 s`; all configurable |
| P2-X22 | Operator resolution: roles, session-age limit, four-eyes scope, accepted evidence types | N2 | owner and platform_admin; session or key at most 15 minutes old; four-eyes for `not_executed` only; E-rejected, E-terminal and E-executed |
| P2-X23 | Whether a replacement call may follow `abandoned_unknown` | N2 | Only as a new call, through an explicit audited command that acknowledges the presumed double cost |
| P2-X24 | Whether billing or usage reconciliation is required | N2 | Not required for N2 |
| P2-X25 | How `store` applies given the Phase 1 ingestion order (X-A, section 7.4) | N1 | Phase 1 upload unchanged; `store` evaluated by Phase 2 operations and `rights.review`; non-allowed artifacts flagged and never processed |
| P2-X26 | Personal data in sources (with L12; GOV-10) | N1, and before any real data | Exclude such sources, flagged in metadata, until a data-protection review |
| P2-X27 | Retention of quarantined copies (C6-2) | N2 | Kept indefinitely in Phase 2; revisit with P2-X9 |
| P2-X28 | Budget ceilings: who sets them, and the default | N2 | Per project, default 0 (no spending until set), set by the project owner |
| P2-X29 | Starter registry content: knowledge types and aspect checklists for research papers | N3, N4 | A versioned pack, importable with `studioctl`, kept outside Go code and never installed automatically |
| P2-X30 | Scale envelope: papers per project, knowledge units, calls per run | N4 | No default; Godwin to state the order of magnitude |
| P2-X31 | Target filesystem for FS1–FS7 (section 6.9) | N0 | The development host's local ext4 |
| P2-X32 | Whether Phase 2 ends in a deployed service | N9 | No; Phase 2 ends in software verified on a single host, and deployment gets its own plan |
| P2-X33 | Commercial intent of releases (drives `use_context` at freeze) | N9 | No default; Godwin to state it |
| P2-X34 | Manual verification calls to a provider, with synthetic prompts only | N2 | Allowed only under the N2 authorisation, opt-in and synthetic |
| P2-X35 | Whether relations across sources are in Phase 2 | N5 | Deferred, except citation links |

Legal questions L1–L12 (section 7.8) are also unresolved.

Phase 1 Unresolved items (Phase 1 section 20.3, X1–X8) are unaffected by this
document.
