# Journal voice chart contract

Implementation in progress; not deployed and not yet wired into model output or request validation.

`TableChart` references existing category and value column IDs, with a stable ID, title and kind (`bar`, `line`, `proportion`). It never stores duplicate values or computed output. Shape limits match the native client: title up to 300 characters, one to eight distinct value columns, category excluded from values, and exactly one series for proportions.

New configurations require existing columns. Missing or unconfirmed data remains a gap; confirmed values must share units. Proportions require complete, nonnegative data with at least one positive value. Existing dangling references remain representable so the user can repair a chart after deleting a column.

Focused verification: `go test ./internal/journal/voice -run '^TestChartReferencesValidateShapeAndDataWithoutInventingValues$' -count=1` passed. Covers JSON round trip, valid references, review-data gaps, invalid proportions, dangling references and category/value conflicts. Mixed-unit, operation, source-partition, global identity and end-to-end protocol tests remain to be added.

Next integration: table context projection/budget and identity validation, sparse add/replace/remove patches, revision authorization, model schema and instructions, and a synchronized client contract check. Preserve unrelated Health behavior and existing deployment work; no deployment is part of this local foundation change.
