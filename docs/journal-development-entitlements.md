# Journal development authorization

The isolated shared test gateway accepts operator-approved development grants in
`/journal-read-aloud/development-grants.json`, alongside the existing private
integration registry. No client header, local simulation or Apple Sandbox expiry
creates a grant. Production configuration cannot enable this registry.

Each grant binds a registered Journal development key and its server device ID,
with fixed `startedAt` and `expiresAt` timestamps no more than 30 days apart.
App Attest registration still requires an admitted App build. Every lookup
reloads the private bounded file, so removing a grant revokes access immediately;
activation and restart never extend its expiry or restart its voice period.
The existing per-App limits, voice allowance, consent gates and project cost
reservation/settlement remain in force.

Grants supply a read-only authorization and quota-period view. They never insert
an Apple transaction, change a purchase binding or overwrite Sandbox/Production
entitlement rows. An active verified purchase takes precedence. Voice, organizing,
illustrations and read-aloud use this same decision; development read-aloud uses
a distinct subject from purchase accounting.

Explicit simulator integration credentials use the existing signed, expiring
registry and replay protection. A separately approved development grant is still
required for voice and organizing; ordinary read-aloud credentials retain their
narrow route allowlist. These requests use real providers, budgets and quotas,
without creating an Apple attestation or purchase row.

Journal cloud AI consent is one all-or-nothing decision dated 2026-11-03. Every
consent request contains the three scopes `managed_subscription`,
`journal_illustration`, `journal_read_aloud` exactly once with the same boolean.
The privacy service rejects partial/mixed/duplicate/old decisions before writing,
and every cloud feature requires the complete current decision. Earlier partial
approvals are never promoted. Health's published document versions are unchanged.

The constrained simulator repository persists one unified decision in the existing
isolated `journal_simulator_read_aloud_consents` ledger, using its historical
read-aloud scope column and the new common document version. It acknowledges all
three requested scopes only after the single write succeeds. All scopes resolve
to that one current choice; it does not create voice/illustration route grants or
paid entitlements. The row expires with the credential and restart cannot recreate
withdrawn consent. Real Apple identities retain their normal transactional,
three-row privacy repository. No schema change, fake Apple key or purchase is
needed. Text organizing retains its durable, expiring nonce store; media-bearing
requests remain denied for constrained simulator credentials.
Simulator private keys are never
compiled into device or Release builds. The test producer removes its credential
and grant when verification ends; expiry and the registry's fixed size cap bound
interrupted runs.

Deploy through `deploy/tencent/platform_test_deploy.py` and retain its matching
source, authenticated backup and recovery evidence. The optional grant file does
not change the existing credential format: a previous service safely ignores it.
Keep a private pre-change copy before operator updates. Removing the optional file
revokes all development grants without deleting journals or purchasing records.

Apple Sandbox purchases remain a separate real transaction test. An empty product
response must not be diagnosed as a tax, bank or agreement failure without account
evidence, and cached development access must never be presented as an Apple purchase.
