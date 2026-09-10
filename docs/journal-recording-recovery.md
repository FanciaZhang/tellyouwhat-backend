# Journal recording task loss

An acknowledged `processing` task that the provider reports missing after the existing 30-second propagation grace now becomes terminal `failed` with `analysis_task_missing`. The original provider task ID is retained. Further process requests return the terminal state without another query or submission; the existing revision-fenced retry endpoint refuses to resubmit this acknowledged task.

Uncertain submissions continue to use `analysis_submission_missing` and the existing explicit, same-ID, query-first retry path. Network/query errors alone never authorize resubmission. Owner isolation, budget reservation, expiry and source hashing are unchanged.

The App may offer an explicitly confirmed new analysis request while retaining its local recording, original transcript and edited document. This is a new potentially billable operation, not an automatic polling side effect.

Verified `go test ./internal/journal/voice ./internal/journal/development`, targeted recording tests, and recording tests with `-race`. No deployment or remote push performed.
