# Background job token accounting

A job capability records an authenticated request binding without debiting daily
or monthly tokens. Preparation still checks rate limits and available capacity.
The worker atomically reserves tokens immediately before provider execution and
checks the current limits again. An unused capability or a job cancelled while
queued consumes no tokens. A cancellation observed after reservation but before
the provider call reconciles that attempt to zero.

Version 2 reservations distinguish prepared bindings from charged execution
attempts. Concurrent delivery reserves each attempt once. Actual provider usage
settles only the attempt's recorded daily and monthly windows. A provider call
with unknown usage retains its conservative reservation. A provider protection
deferral releases its debit and must reserve current capacity on resumption.
Legacy version 1 prepayments remain readable and use their original windows.

## Deployment

Upgrade every gateway and worker that can dispatch or claim jobs before issuing
version 2 capabilities in production. A shared outbox can be dispatched by either
blue/green slot; the old slot must not claim new jobs. Old workers reject version
2 reservations rather than execute without a debit. Use a coordinated cutover
or drain the old job dispatchers and workers before enabling the new gateway.
Do not roll an old worker back into a queue containing version 2 jobs.

## Existing unclaimed prepayments

Build `go build -o quotarepair ./cmd/quotarepair` from the verified backend commit.
Run it in the backend's existing secret-bearing runtime with `DATABASE_DSN` and
`REDIS_URL`; do not put credentials in command arguments or logs.

`quotarepair` defaults to read-only preview. It derives job-capability reservation
keys from durable Health request bindings created within the previous 26 hours,
with a maximum of 10,000 records. It reports only aggregate reservation and token
counts. Synchronous reservations use a different key namespace.

After reviewing the preview and completing the coordinated upgrade, run
`quotarepair --apply`. The Redis transaction converts an unchanged version 1
reservation only if no worker attempt has claimed it and it is not settled.
The conversion refunds its original counters once, preserves the request binding
and expiry, and requires a new execution debit if the client later uploads.
A concurrent worker claim prevents conversion. Re-running the command is safe.

Expired or unbound records and reservations from workers that did not record
attempt ownership need separate reconciliation; this tool cannot infer actual
provider usage for them. It does not change usage-ledger entries, provider bills,
subscription limits, or free recognition session counts.
