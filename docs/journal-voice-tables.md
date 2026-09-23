# Journal voice table creation contract

The local Journal voice protocol is `journal-voice-v10`. This protocol is not deployed. The iOS client must adopt `tableCreations` and table source partitions before the pair is deployable.

`tableCreations` is a required array in model revisions. Each creation carries independent operation, block and table UUIDs, an existing `afterID` or null, the instruction source and exact instruction, a title, stable column IDs and stable row IDs. Each cell addresses its column and carries a flat kind (`text`, `number`, `pending`), text, decimal string, unit, estimate flag, review flag and quoted source anchors. Unused strings are empty. The App attaches its local recording identity and repeats source and document validation.

Creation is a proposal for a standalone table. Existing prose remains present. The App owns confirmation, dismissal, insertion and undo. A revision can propose at most four tables; each has at most 16 columns, 64 rows and 512 cells. Across the revision, cell values contain at most 20,000 Unicode code points. These are bounded generation batches, not a limit on the persisted table model.

Each populated cell must cite uniquely located data from pending utterances consumed by this revision. Sources must fall within a `content` partition associated with the proposed block. The exact creation instruction is an `instruction` partition associated with the same proposed block. Table sources are not repeated in `passages`, which continues to describe prose. Missing values remain `pending`, numeric precision and units stay explicit, and estimated/review states survive the wire. Structural moves, splits and merges use a separate revision; independent prose and formatting may continue.

The server verifies shape, identities, bounded data, decimal syntax, source existence, unique quote/context resolution and partition coverage. Semantic truth and appropriate column assignment still require model evaluation and user review. App-side validation is authoritative for grapheme boundaries, current document conflicts, source archives and saved user changes.

Pending work includes iOS wire adoption, source consumption and preview provenance, retrieval of relevant earlier utterances, updates to existing tables, spoken confirmations, and real ASR/model end-to-end acceptance. No shared Health paths or deployment configuration were changed for this contract.

Local validation: `go test ./internal/journal/voice ./internal/journal/service` passes. Table tests cover evidence, partitions, decimal format, absent values, row/column identities, destination conflicts, schema requirements and bounded generation.
