# Background AI result delivery

Health submits a durable job with its existing digest-bound upload capability.
Clients opt in with `X-Health-Job-Result-Delivery: download-v1`. Only opted-in
capability responses include `resultToken`, with the same expiry. This is a separate read-only credential: it cannot submit work, and the
upload credential cannot read results. Tokens must stay out of URLs and logs.

`GET /v1/ai/jobs/{id}/result` accepts `X-Health-Job-Result-Capability` and a fresh
`X-Tellyouwhat-Request-ID`. It waits for at most four minutes and returns the
existing `AIJob` representation. Queued/running responses can be downloaded
again; the client must preserve the remote job and request IDs rather than
submit another inference. Connection cancellation stops the wait, not the job.
Responses are `no-store`. Every wait iteration rechecks capability expiry and
the job's owner, app, device, request identity, and durable state. Deleting the
owner key cascades to `ai_jobs`, so the capability cannot recover deleted data.

The app persists the result-download binding before starting a background
`URLSessionDownloadTask`. Its completion passes through the same schema
validation, durable response replay, idempotent model mutation, and terminal
state handling as foreground retrieval. The system completion callback waits
for result processing and surface reconciliation. Network retries download the
same job, with a bounded retry count; they do not consume another inference.

## Compatibility and rollout

Deploy the additive gateway contract before distributing the updated client.
Older clients receive the unchanged three-field capability response; their
closed-schema decoders never see `resultToken`. Updated clients retain foreground polling
for old jobs and gateways that do not provide it. That compatibility path does
not provide result retrieval while iOS suspends the app.

Verify a real subscribed device in foreground, after switching apps, and after
locking the screen. Confirm one persisted result, one notification, and current
meal rows and energy totals. Verify delayed/repeated callbacks, network loss,
app restart, and deletion. A simulator test or a gateway HTTP test alone does
not prove iOS background scheduling on the distributed app.

## Request construction boundary

Result delivery consumes job identity and a result capability, independently
of prompt text or its implementation language. Prompt construction remains in
the existing request-artifact path. A future server-owned prompt builder can
produce the same durable job/result contract without changing iOS result
retrieval. Direct-provider requests retain their existing prompt builder and
background upload path.
