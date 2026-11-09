# Journal voice table creation contract

The local Journal voice protocol is `journal-voice-v16`. This protocol is not deployed. The iOS client uses v16 and adopts table edits and spoken resolutions through source-linked confirmation previews. Real-provider and device acceptance remain pending.

`addCalculation` targets a table ID and supplies `calculation: {id, title, kind, columnID, rowIDs}`. `sum` requires an empty row list; `difference` requires two distinct row IDs in minuend/subtrahend order. No computed value is accepted. Other payloads are empty or null. The request's `calculations` context carries existing expressions without cached results or historical quotes. IDs use lowercase UUID strings. Expression titles count toward the existing context budget; a table holds at most 64 expressions. Existing dangling operands remain representable, whereas new calculations must have compatible confirmed numeric operands and matching units. Sums exclude pending/review rows and require at least one confirmed value. Differences require both operands. Source instruction partition, identity reservation, preview confirmation and undo remain mandatory. Arithmetic is performed by the client's deterministic Decimal calculator; server validation does not manufacture or persist results.

## Calendar dates

Cell kind `date` stores a complete Gregorian date in `text` as strict `YYYY-MM-DD`. Number and unit are empty; approximate is false. Calendar-invalid dates and unused payload are rejected. The same encoding applies to creation, sparse cell edits and source-free table context; creation/edit evidence authorization still applies. Incomplete and uncertain spoken dates remain text or require clarification instead of inventing a year or precision. Dates are never numeric inputs to sums or numeric sorting.

The date validator matches the native picker boundary: Gregorian-invalid leap days and the 1582-10-05 through 1582-10-14 reform gap are rejected rather than normalized. Such historical expressions can remain text.

`sortDatesAscending` (earliest first) and `sortDatesDescending` (latest first) address a date column with `targetID`; all other fields are empty or null, including `order`. Both runtimes sort complete typed dates deterministically, retain equal-date order, and keep pending/review-required rows stably at the end in either direction. Confirmed text and numbers are rejected rather than interpreted as dates. Whole rows, identities, values and source anchors remain intact. The existing source-partition, preview, persistence, conflict and undo pipeline applies. The model selects the column and direction, not the row permutation.

## Deterministic numeric sorting

`sortNumbersAscending` and `sortNumbersDescending` patches address a numeric column with `targetID`. All other payload fields are empty or null, including `order`. The model identifies intent, not the resulting row permutation. Server validation uses exact rational decimal comparisons; the App uses Decimal and the same stable ordering rules. Equal values retain their original order. Pending and review-required cells remain at the end in their original order in both directions. Approximate values keep their flag. Confirmed text or mixed units are rejected; there is no implicit unit conversion or date parsing. Source-partition authorization, preview confirmation, persistence and undo use the existing edit pipeline. No-op proposals are rejected. This protocol change is Journal-scoped and has not been deployed to the shared service.

`tableCreations` is a required array in model revisions. Each creation carries independent operation, block and table UUIDs, an existing `afterID` or null, the instruction source and exact instruction, a title, stable column IDs and stable row IDs. Each cell addresses its column and carries a flat kind (`text`, `number`, `date`, `pending`), text, decimal string, unit, estimate flag, review flag and quoted source anchors. Unused strings are empty. The App attaches its local recording identity and repeats source and document validation.

Creation is a proposal for a standalone table. Existing prose remains present. The App owns confirmation, dismissal, insertion and undo. A revision can propose at most four tables; each has at most 16 columns, 64 rows and 512 cells. Across the revision, cell values contain at most 20,000 Unicode code points. These are bounded generation batches, not a limit on the persisted table model.

Each populated cell must cite uniquely located data from pending utterances consumed by this revision. Sources must fall within a `content` partition associated with the proposed block. The exact creation instruction is an `instruction` partition associated with the same proposed block. Table sources are not repeated in `passages`, which continues to describe prose. Missing values remain `pending`, numeric precision and units stay explicit, and estimated/review states survive the wire. Structural moves, splits and merges use a separate revision; independent prose and formatting may continue.

The server verifies shape, identities, bounded data, decimal syntax, source existence, unique quote/context resolution and partition coverage. Semantic truth and appropriate column assignment still require model evaluation and user review. App-side validation is authoritative for grapheme boundaries, current document conflicts, source archives and saved user changes.

Pending work includes retrieval of relevant earlier utterances, updates to existing tables, spoken confirmations, and real ASR/model end-to-end acceptance. No shared Health paths or deployment configuration were changed for this contract.

Local validation: `go test ./internal/journal/voice ./internal/journal/service` passes. Table tests cover evidence, partitions, decimal format, absent values, row/column identities, destination conflicts, schema requirements and bounded generation.

## Existing table context

`Snapshot.tableContext` projects selected existing tables into model input. Each item carries its owning `blockID`, existing `tableID`, title, ordered columns and ordered rows with flat cell values. Ownership must match `blockComponents`. Column and row identities are unique in the supplied context and cannot collide with known block or component identities. Display order and decimal spelling are retained exactly; empty manually entered text and an empty existing table are valid document states.

The context budget is four tables, at most 16 columns, 64 rows and 512 cells per table, and 20,000 Unicode code points across titles, column titles and cell content. Oversized context is rejected, not silently truncated into an apparently complete table. The App now selects bounded whole tables using local lexical relevance; semantic selection and retrieval for larger tables remain pending. This is a per-request context budget, not a persistence limit.

Cell `sources` must be empty: historical recording quotations are not needed to address an existing cell. Current values are document state, not authorization or evidence for a new edit. The prompt explicitly separates existing tables from new creation requests. This input support alone does not enable existing-table mutation: the edit output protocol and command application still need wiring.

The shared value validator preserves the stricter creation rule requiring nonblank generated text. Focused tests cover exact model projection, row/column ordering, decimal precision, review/estimate flags, missing values, empty tables, ownership, identity collisions, shape, historical-source payload rejection and aggregate context limits. Journal voice and service tests pass; nothing has been deployed.

## Sparse edit materialization

The local `TablePatch` core supports setting a cell, renaming a table or column, inserting/deleting rows and columns, and explicitly ordering rows/columns. Targets use existing stable identities. Insert targets mean “after this row/column”; an empty target inserts at the beginning. A new column starts with pending cells, which subsequent cell operations can fill. Ordering must be an exact permutation. A batch has at most 128 operations, validates each intermediate shape/budget, rejects unused payload fields and never reuses a deleted identity within the batch.

Materialization copies the request table and preserves untouched values and evidence. Failure returns no partial result and does not mutate the request. The v11 model contract exposes these patches in required `tableEdits`; App decoding, source validation, preview, confirmed application and undo are wired and covered by focused local tests. Spoken confirmation and real-service acceptance remain pending.

Tests exercise combined numeric/structural changes, exact decimal and evidence preservation, insertion/reordering/deletion, snapshot immutability, missing or irrelevant payloads, incomplete/duplicate permutations, invalid targets, identity reuse and invalid table shapes. `go test ./internal/journal/voice ./internal/journal/service` passes. No deployment or Health behavior changes.

## Existing-table revision authorization

Each edit contains a new operation `id`, existing `blockID`/`tableID`, pending `sourceID`, exact `instruction` and sparse `patches`. At most four distinct context tables may be edited per revision. The validator checks ownership, operation/new-row/new-column identity collisions, instruction consumption and partition linkage. It rejects no-op results, including changes to evidence alone. Existing-table edits are separated from creation and other structural operations; the owning block cannot also receive a prose rewrite.

Every supplied populated cell has current quoted evidence. The quote must uniquely locate either inside that edit's instruction or in a content partition linked to the same owning block. Thus “把价格改成49.90元” can remain one instruction, with “49.90元” providing value evidence without duplicating the command into prose. Unrelated context, invented quotes and duplicate evidence are rejected. Schema and prompt define nullable unused payload objects and exact row/column targeting; semantic correctness still requires model evaluation and user review.

Focused tests cover full revision validation, source partitions, instruction-embedded and separate content evidence, invalid ownership/identities/quotes/targets, missing consumption, no-ops and required schema fields. No provider integration or deployment has been performed.

## Spoken table resolutions (v12)

`tableReceiptContext` carries at most eight recent operation receipts, separate from table content: receipt, table and block IDs, creation/edit kind, proposed/applied/undone state, title, instruction summary and `canConfirm`/`canUndo` flags derived by the App from native plan feasibility. Proposed creations can reference a block not yet inserted. Stale proposed receipts remain dismissible; undoable applied items and confirmable edits require an existing correctly owned table.

Required output `tableResolutions` supports confirm, dismiss and undo with a fresh operation ID, exact current instruction, source ID and existing receipt ID. Reconfirmation of an undone item requires explicit user intent and current capability. Validation rejects unavailable capabilities, duplicate receipt/table targets, missing consumption/partitions, fabricated instructions, identity collisions and conflicting operations. Confirmation words are attributed as instructions to the affected block, not rewritten into prose. The prompt requires clarification for ambiguous confirmations rather than selecting an arbitrary preview.

`go test ./internal/journal/voice ./internal/journal/service` passes, including receipt projection, lifecycle, proposed creation, stale dismissal, invalid state/capability/ownership, source evidence and schema tests. App receipt projection, persisted resolution history, application, undo and redo are integrated and covered by focused local tests. Spoken UI journeys, real-provider and device acceptance remain pending. No deployment occurred.
