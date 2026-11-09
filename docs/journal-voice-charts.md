# Journal voice chart contract

Implementation in progress; not deployed. Context validation, sparse edits, model output schema and instructions are wired locally. Client/full-revision integration and real model acceptance remain pending.

`TableChart` references existing category and value column IDs, with a stable ID, title and kind (`bar`, `line`, `proportion`). It never stores duplicate values or computed output. Shape limits match the native client: title up to 300 characters, one to eight distinct value columns, category excluded from values, and exactly one series for proportions.

New configurations require existing columns. Missing or unconfirmed data remains a gap; confirmed values must share units. Proportions require complete, nonnegative data with at least one positive value. Existing dangling references remain representable so the user can repair a chart after deleting a column.

Focused verification: `go test ./internal/journal/voice -run '^TestChartReferencesValidateShapeAndDataWithoutInventingValues$' -count=1` passed. Covers JSON round trip, valid references, review-data gaps, invalid proportions, dangling references and category/value conflicts. Mixed-unit, operation, source-partition, global identity and end-to-end protocol tests remain to be added.

Context validation now includes chart shape, title budget and identity claims. Sparse `addChart`, `replaceChart` and `removeChart` preserve input rows and clone configuration slices; additions require a new ID, replacements retain the existing ID, and removal retains the original table. New and replaced configurations must resolve. Unused chart payloads are rejected. Revisions require the existing instruction partition authorization. Timeline and journey identity reservation includes chart IDs.

Verification: `go test ./internal/journal/voice -run 'Test(Chart|Calculation|TableContext)' -count=1` and `go test ./internal/journal/voice -count=1` passed. New tests cover add/replace/remove, input nonmutation, missing instruction authorization, duplicate identities and historical dangling references. These deterministic module tests are not live model or deployed service acceptance.

Model schema requires a nullable `chart` payload for every table patch and exposes add/replace/remove. Chart objects are strict and contain only the five reference/configuration fields. Instructions distinguish comparison, trend and proportion, preserve gaps and units, request clarification for ambiguous targets, and keep commands out of prose. A local HTTP model double verifies request projection, response decoding and revision authorization end to end within the server. `go test ./internal/journal/voice -count=1` passes including schema and transport tests; this does not measure real model quality.

Local protocol is now `journal-voice-v23`, synchronized with the native transport source. The gateway distinguishes an empty consent-version field (`voice_consent_required`) from a nonempty mismatched protocol (`voice_protocol_mismatch`); actual privacy consent retains its existing separate gate. Gateway admission/protocol tests pass after correcting the new fixture's request ID to match the fake authenticator. Voice module tests also pass. The matching native build is still being verified. Deploy the coordinated client/server pair only after acceptance; neither a version bump nor local tests prove production availability.

Next integration: real model/service acceptance and broader cross-type revision identity fixtures. Preserve unrelated Health behavior and existing deployment work; no deployment is part of this local change.
