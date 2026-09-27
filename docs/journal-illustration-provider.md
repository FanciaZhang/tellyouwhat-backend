# Journal illustration provider boundary

The isolated `internal/journal/illustration` package is not registered with the
gateway and does not change Health routes, quota defaults, or text-token billing.

The provider implements the [official Ark image API](https://docs.volcengine.com/docs/ark/image-generation-api?lang=en)
using a configured image model, one image per request, base64 output, and enabled
AI watermark. A confirmed reference image is sent inline, never fetched from an
arbitrary client URL. Results preserve original bytes; dimensions are inspected
from PNG/JPEG headers rather than trusted from provider JSON. The App's ImageIO
boundary performs actual bounded preview decoding before document acceptance.

## Safety and verification

- HTTPS endpoint; redirects disabled; three-minute timeout; bounded response,
  image bytes, dimensions, and pixel count.
- One upstream attempt. Transport/read failures and 5xx return an unknown outcome;
  automatic resubmission is not authorized by that result. Invalid outputs also
  do not establish that no generation was charged.
- Errors omit prompts, images, credentials, and provider response bodies.
- `go test ./internal/journal/illustration` covers request format, reference
  preservation, header dimensions, malformed output, invalid input, redirects,
  quota responses, 5xx, and transport failures with an injected transport.
- No live provider or paid-generation call has been made.

## Required before enabling

`task.go` now defines frozen confirmation digests (including reference-image
hash and consent revision), authenticated app/owner identity matching, revisioned
state transitions, expiration, and separate delivery/charge states. Cancellation
before dispatch releases the reservation; cancellation after dispatch retains
uncertain charging until a result is reconciled. Late results never restore a
cancelled or expired delivery. HTTP rejection alone does not establish billing.
Tests cover both cancellation/completion orderings, stale transitions, material
changes, app/owner separation, and reconciliation without redispatch.

The MySQL `IllustrationRepository` now implements encrypted CreateOrGet, a
transactional row-locked Claim, and cancellation with payload erasure. Migration
0005 adds only a dedicated Journal table and is not applied to any live database.
App, owner and task identity are bound into AES-GCM associated data. Duplicate
admission preserves the original model and input. Claim commits the dispatch
fence before returning private input and never reclaims an already started task.
An unknown commit result must be reconciled, not dispatched speculatively.

Local compilation, encryption-isolation and embedded-migration tests pass.
`TestIllustrationDurableClaimAndCancellation` covers eight concurrent claimers,
repository reconstruction, private payload erasure and scoped identity, but was
**skipped locally because MYSQL_TEST_DSN is not configured**. Run it against an
isolated `_test` MySQL database before declaring persistence/concurrency verified.
The test deletes only its own UUID row, not existing tables or user data.

Production still needs a worker, authorization checks at admission and dispatch,
atomic image quota reservation/settlement, encrypted results, expiry maintenance,
and delivery. The repository is not registered with the gateway. A durable claim
alone is not a complete generation workflow; a crash after claim but before HTTP
dispatch is conservatively unresolved and must not auto-generate a second image.

Wire a Journal-only authenticated task contract with explicit material consent,
image-denominated reservation/settlement, durable request identity, ambiguous
outcome reconciliation, cancellation, and encrypted short-lived result delivery.
Connect the App's confirmed brief/version to that contract and verify real-model
quality and device flows. Cancelling local waiting must not promise that upstream
generation or charging was cancelled. This provider alone is not a shipped image
generation feature.
