# Shared-gateway Journal illustrations

`cmd/gateway` now implements the existing create/status/result/cancel illustration contract. It uses Journal App Attest identity, managed entitlement and both managed-AI and illustration consent. Cancellation remains available to the authenticated owner after entitlement or consent withdrawal. Private developer connection credentials are not accepted by these handlers; native-client authentication migration remains required before switching the test domain.

Enable only after migration `0065_journal_illustration_jobs.sql` and reviewed environment configuration: `JOURNAL_IMAGE_ENABLED=true`, `JOURNAL_IMAGE_BASE_URL`, `JOURNAL_IMAGE_API_KEY`, `JOURNAL_IMAGE_MODEL_ID`, and a positive `JOURNAL_IMAGE_MAX_COST_NANOS` no greater than CNY 1. MySQL and project cost controls are required. The feature defaults off. The shared project/App budget and concurrency controls apply; the generation attempt retains its conservative reservation because the provider response is not an invoice.

Tasks, approved prompts, references and results are encrypted using the existing payload cipher and bound to the authenticated owner/request. A durable compare-and-swap claim precedes generation, so overlapping release processes cannot dispatch the same task twice. An interrupted running task becomes uncertain and is never automatically regenerated. A temporary entitlement/consent/policy lookup failure defers the queued task for 30 seconds without blocking later jobs or discarding input. A model change cancels undispatched old-model work.

Cancellation or the 24-hour delivery deadline clears input/results while preserving the idempotency tombstone and charge state. Scheduled maintenance expires content even when generation is disabled. Account deletion cascades to illustration rows and fences late writes. Known provider outcomes may update a cancelled task's charge but cannot restore its image. Raw material must not enter logs or management metrics.

Withdrawing unified Journal cloud AI consent also cancels the authenticated owner's queued and active illustration jobs and clears stored inputs and results in the consent transaction. Admission holds the current grant stable through insertion. A request that began before a new grant cannot use it to enqueue, and replay of a cancelled request returns the cancelled task. The worker checks its claim immediately before dispatch. A provider call already dispatched can finish and settle its charge, while its image remains unavailable.

The queue follows release standby/activation/pause/drain, and shutdown waits for background settlement before closing the database. HTTP admission bounds concurrent image body/result allocations. Management operations include a separate illustration pause and queue counts. These controls share the gateway process; this change does not introduce another API deployment.

Local verification covers handlers, authorization, ownership, replay/conflicts, admission, price validation, budget outcome recording and management UI. Seven real temporary-MySQL race tests cover encryption, atomic claims, overlapping workers, late cancellation, crash/expiry, account deletion, disabled-feature maintenance, release drain and queue fairness. Provider calls in these database tests are synthetic; no real cloud generation through the shared gateway has been verified.

Not yet deployed. Existing private service data and credentials remain intact. Native-client App Attest migration, recording-file import, private-state disposition, environment-specific secrets, release/backup integration and real device/cloud acceptance remain before retiring the private API. See `evidence/journal-shared-illustrations-2026-10-16.json` for source and test evidence.

## 2026-11-09 shared-test withdrawal candidate

The active shared-test selection changed from `1127323a` to
`2333740ab1fe5f1fb85aa46e61895a2dc5c92bfc` before staging. The preflight stopped
before any runtime mutation. The latest registered archive was verified and
imported as snapshot `56872b0d`; all 1,348 file contents and executable modes are
preserved, without claiming the unavailable original Git commit metadata. The
earlier withdrawal change from `465f0b8` and the request-context fix are applied
to this current baseline, preserving the other applications,
configuration, routes and migrations. The consent transaction clears admitted
illustration payloads and fences late provider results while retaining settlement.

HTTP/MySQL race checks exposed pooled Gin contexts retained by consent withdrawal
and illustration admission. Platform authentication, entitlement, privacy and
illustration services now receive the underlying request context. Read-aloud
reuses the same existing helper, and a deterministic test preserves request
cancellation and values after Gin reuses its wrapper. The latest baseline's
guardian-scope migration replay checks are preserved without the older candidate's
fixture adjustment. Six affected packages passed 277 test/subtest events, 45
targeted race checks passed, and vet passed. Four explicit Redis/App Attest recovery
and real image-provider checks remain skipped in the normal local run; no
simulated purchase authorizes a provider call. Full results and both failed race
attempts are retained in the local
`journal-readiness/cloud-media-2026-11-09` evidence directory.

This candidate is locally verified. Deployment and real App generation are separate
steps; this paragraph does not declare either complete or change production.
