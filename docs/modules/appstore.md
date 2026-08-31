# App Store subscription module

## Responsibility

`internal/appstore` owns the production-only trust boundary between StoreKit, App Store Server API, and the managed-AI entitlement store. It verifies Apple-signed transaction and notification JWS values, asks Apple for current subscription status, and returns a normalized subscription state. It does not authenticate Health App devices and does not grant AI access directly.

## Public surface

- Verify a StoreKit transaction JWS received through an App Attest-authenticated request.
- Resolve current subscription status from App Store Server API using a verified transaction identifier.
- Verify and normalize App Store Server Notifications V2.
- Create the short-lived ES256 bearer token required by App Store Server API.

`internal/entitlement` consumes this surface to bind or refresh an original transaction for an attested key. `internal/gateway` only decodes HTTP, invokes the entitlement service, and maps typed errors.

## Dependencies

Allowed dependencies are the Go standard library, configured Apple root certificates, an injected HTTP client, and the existing entitlement store interface. The package must not import gateway, App Attest, Ark, quota, media, jobs, or storage adapters.

## Security invariants

- Accept only ES256 compact JWS with the three-certificate `x5c` chain Apple documents and uses in its official server libraries.
- Require the Apple receipt-signing leaf OID and WWDR intermediate OID, validate the chain to configured Apple roots, and verify the JWS signature.
- Require the configured bundle ID, App Apple ID in production, environment, and `health.ai.subscription.monthly` product ID.
- A client transaction is evidence for an identifier, not proof of current access; App Store Server API remains the source of current expiry/revocation status.
- Notification processing is idempotent by Apple `notificationUUID`; the nested signed transaction is independently verified before storage changes.
- No JWS, private key, subscription payload, or transaction identifier is written to ordinary logs.

## Error model

Malformed or untrusted Apple data is a permanent verification denial. Apple network/rate-limit/server failures are retryable service-unavailable errors. A valid but inactive subscription produces a forbidden entitlement result. Duplicate notifications acknowledge success without applying a second mutation.

