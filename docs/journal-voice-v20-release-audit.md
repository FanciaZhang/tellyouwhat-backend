# Journal voice v20 release integration

## Verified baseline — 2026-09-24

- Production is stable on `8dd188b83e8fa6103e04b57f7d7ba9411531e022`, green slot. Health and Journal public HTTPS readiness both returned `status=ready`.
- The requested voice candidate is `b4953af`, developed from an older common ancestor. It has 35 commits not in origin/main; origin/main has 144 commits not in the candidate.
- Relative to the common ancestor, candidate changes are scoped to Journal voice, one development-server test, and Journal documentation (48 files). Deploying its entire old checkout would nevertheless regress newer shared services and deployment tooling.
- Integration is isolated in branch `release/journal-voice-v20`, based on the verified production/main baseline. The original dirty checkout is untouched.
- No remote branch was pushed, no deployment was activated, and no production configuration or database was changed.

## Integration gates

The merge has six semantic conflicts: `asr.go`, `budgeted.go`, `model.go`, `session.go`, `types.go`, `voice_test.go`. Do not resolve by selecting either complete side.

1. Preserve current production speech normalization, provider evidence diagnostics, timing/window bounds and speaker parsing while adding stable incremental utterance identities and acoustic evidence.
2. Preserve configured model/prompt parameters, prepared-request cost reservation, actual usage settlement and error diagnostics when adding the v20 structured revision schema.
3. Retain lifecycle tracking for model requests and WebSocket work. Validate consumed-source acknowledgment against actual applied document state; revision equality alone must not authorize consumption.
4. Reconcile the recording-analysis preview API, current writing-style/manual-edit contracts and existing production tests with incremental snapshots. Existing imported-recording and dialogue behavior must not be silently removed to make the new real-time protocol compile.
5. Retain all shared Health, quota, storage, lifecycle, migration and administration changes from main. No schema migration is introduced by the original voice candidate.
6. Run focused Journal tests first, then the current main CI gates including MySQL/Redis, generated contracts, deployment protections and six service images on the exact integrated commit.
7. Use main's current blue-green deployment, not the candidate's older replacement script. Preserve rollback and drain checks; capacity rejection is not authorization for disruptive deployment.
8. Verify production image/commit, slot state, both public readiness endpoints and scoped business checks. Readiness is not a real-device voice journey.

## Status

Local integration now compiles. Production remains unchanged pending CI and deployment acceptance. Local checks do not establish real-provider or device acceptance.

### Integration checkpoint

- Resolved ASR parsing by retaining production StreamUtterance normalization, rolling-window handling, trace/schema hooks and optional metadata. Added top-level/nested provider evidence fallback without inventing zero values; explicit zero takes precedence over a nested value. Invalid, nonfinite and oversized metadata remain rejected.
- Two isolated Go tests passed, including direct/nested/fallback evidence subcases. Command: `go test internal/journal/voice/stream_utterance.go internal/journal/voice/stream_metadata_test.go -v`. This does not compile the still-conflicted package.
- BudgetedRewriter retains production PrepareRewrite-based model/price/output reservation and actual usage settlement. Full cost-control regression remains pending with model integration.
- Four files remain conflicted: model, session, types, voice tests. StreamUtterance evidence must be converted explicitly to stable identified utterances at the session boundary; do not replace the production normalizer with the older parser.
- Recording-analysis preview uses the production Snapshot/Revision contract, including passage paragraph indices and dialogue validation. Resolve its semantic distinction from incremental real-time editing before choosing shared types; never discard recording review merely to resolve a compile error.

### Integrated candidate verification

- Resolved the remaining source conflicts with separate incremental and recording-review model contracts, while retaining configured parameters, actual usage settlement, provider diagnostics and lifecycle tracking.
- Incremental snapshots also run common validation for vocabulary, manual edits and writing-style identifiers. Paragraph/component metadata is validated even when no new utterances are pending.
- Source consumption requires a document revision at or beyond the offered result, the consumed IDs in known sources, and their absence from pending sources. A manual edit reaching the expected revision is not an acknowledgment. Socket regression tests now send the same source acknowledgment fields as the current App.
- `go test ./... -count=1 -timeout 120s` passed on the integrated checkout. External MySQL/Redis and opt-in live-provider tests were not enabled locally; those layers remain separate.
- `go vet ./...` and `git diff --check` passed.
- `go test -race ./internal/journal/voice -count=1 -timeout 90s` passed, including recording review, realtime source acknowledgment, style fencing, structured tables/timelines, metadata parsing and privacy-safe diagnostics.
- No Health implementation, migration, workflow, deployment script or production configuration is changed relative to the current main baseline.
