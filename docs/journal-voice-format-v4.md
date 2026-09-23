# Journal voice v4: explicit formatting

This journal-only protocol revision adds a required `formatCommands` array to model revisions. The app consent version is `journal-voice-v4`; deploy the matching app and backend together.

Commands contain a stable UUID, target block UUID, exact quote with optional adjacent prefix/suffix, pending source UUID, exact spoken instruction, a closed mark enum, and an enabled boolean. Marks are bold, italic, underline, strikethrough, yellow, blue, and sage. They describe document operations and never execute tools or arbitrary instructions.

Validation bounds command counts and text lengths, rejects duplicate IDs, unknown/media-only targets, unsupported marks and forged source evidence. Explicit user commands may style manually edited text; autonomous prose replacement retains its existing manual-edit fence. All source IDs remain subject to the normal consumed-source contract.

The client resolves full Unicode character boundaries, persists ambiguous targets for user confirmation, records before/after attributed fragments, and provides property-specific undo. Model output is validated again on-device. Quoted, hypothetical and reported instructions remain prose according to the editor prompt.

Local tests: `go test ./internal/journal/voice ./internal/journal/service`. This change does not modify Health endpoints or authorize production deployment. Client UI, local protocol validation and actual model/service integration are separate acceptance layers.
