# Journal development access

`cmd/journaldevserver` is an independently deployed development process. It is
absent from the production gateway dependency graph and production image build
commands. Health code, storage and commerce are not changed.

The authenticated HTTPS route `/_development/journal/*` on the existing Journal
site forwards to loopback port 18787. The Health site has no development route.
Never expose port 18787 directly. A random operator-provisioned credential is
required for every business request; WebSockets use the normal expiring one-time
tickets. Release clients never contain this credential or simulation code.

The connection credential remains valid until explicitly rotated or the service
is stopped. There is no connection deadline or timer that shuts down the service.
The app persists its simulation mode and original start date. Authenticated
requests carry an installation UUID, mode and start date; the development server
constructs isolated entitlement records from those claims. Forced subscription
stays active until manually changed; monthly simulation expires after one real
calendar month using the same clamped-month calculation as voice subscription
periods. Reopening the app or restarting the service never renews the month.
Expired simulation denies managed operations. Real purchases use the production
service and normal App Attest/Apple validation instead of simulation headers.

These claims are intentionally authoritative only for an installation holding
the private development credential. They cannot grant production or Health
rights. There is no hardcoded device allowlist or manual production SQL grant.
The previous one-off SQL entitlement was removed; no such operator remains.

The launcher preserves credentials on `start`, explicitly revokes them on
`rotate`, and supports `connect --device` without rebuilding. A one-time file in
the device cache is consumed into Keychain and deleted on the next app launch,
so a locked phone does not lose its provisioning step. Provider keys remain on
the server. Debug and Release both require HTTPS and the client rejects credential
redirects.

Voice uses the regular 120-minute monthly allowance. The old 10-minute/CNY 1
session limits are removed. A separate CNY 100 calendar-month estimated spend
ceiling is configured by the launcher; the command reads
`JOURNAL_DEVELOPMENT_MONTHLY_BUDGET_CNY`. The normal budget accounting implementation
is backed by an atomic, private operation history in `JOURNAL_DEVELOPMENT_STATE_DIR`.
This persists reservations and settlements across restart/credential rotation,
conservatively retains unsettled costs, and never stores prompts, audio or output.
Provider billing remains authoritative. Temporary voice sessions, receipts and
ordinary quota counters remain in memory; these development counters rebuild on
restart, whereas the spending ceiling cannot be reset by restarting.

The container runs non-root, read-only except its own state mount, drops all
capabilities, and has CPU/memory/pid limits. `unless-stopped` restores it after
restart. Persistent executable/state paths belong only to Journal development.
Production DB/Redis configuration is rejected and Health/Apple secrets are never
forwarded. Updating it does not restart shared gateway/worker/admin containers.

Focused tests: `go test -race ./cmd/journaldevserver ./internal/journal/development
./internal/entitlement ./internal/costcontrol`. Tests cover invalid credentials,
forbidden routes, missing consent, one-time tickets, forced access beyond 24
hours, expired monthly access, clamped month boundaries, service restart,
persistent/reserved costs and calendar-month budget renewal. Live acceptance uses
only synthetic audio unless the user explicitly tests their own recording.
