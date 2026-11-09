# Journal voice v4: explicit formatting

This journal-only protocol revision adds a required `formatCommands` array to model revisions. The app consent version is `journal-voice-v4`; deploy the matching app and backend together.

Commands contain a stable UUID, target block UUID, exact quote with optional adjacent prefix/suffix, pending source UUID, exact spoken instruction, a closed mark enum, and an enabled boolean. Marks are bold, italic, underline, strikethrough, yellow, blue, and sage. They describe document operations and never execute tools or arbitrary instructions.

Validation bounds command counts and text lengths, rejects duplicate IDs, unknown/media-only targets, unsupported marks and forged source evidence. Explicit user commands may style manually edited text; autonomous prose replacement retains its existing manual-edit fence. All source IDs remain subject to the normal consumed-source contract.

The client resolves full Unicode character boundaries, persists ambiguous targets for user confirmation, records before/after attributed fragments, and provides property-specific undo. Model output is validated again on-device. Quoted, hypothetical and reported instructions remain prose according to the editor prompt.

Local tests: `go test ./internal/journal/voice ./internal/journal/service`. This change does not modify Health endpoints or authorize production deployment. Client UI, local protocol validation and actual model/service integration are separate acceptance layers.

## Spoken follow-up

The unreleased v4 contract also requires `formatResolutions` in each revision. Snapshot `formatContext` offers recent actionable receipts and app-generated candidate IDs. A resolution selects one candidate, undoes a safe applied operation, or dismisses a pending one. Each resolution carries its own ID and exact evidence from a pending utterance.

Context is limited to eight receipts and sixteen candidates, with bounded quotes/excerpts. The server forwards this context without recomputing candidate order. Validation rejects invented IDs, mismatched receipt states, duplicate operations, missing evidence, and rewriting the same target in the same batch. Ordinary narrative and reported instructions remain prose; direct follow-ups are editorial evidence rather than body text.

The client repeats validation against the original snapshot, rejects stale text fingerprints, retains resolution provenance, and advances the revision once per atomic batch. Replayed completed resolutions are idempotent. These changes remain local until the matching app/backend pair is explicitly deployed.
