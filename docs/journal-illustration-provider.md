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

Wire a Journal-only authenticated task contract with explicit material consent,
image-denominated reservation/settlement, durable request identity, ambiguous
outcome reconciliation, cancellation, and encrypted short-lived result delivery.
Connect the App's confirmed brief/version to that contract and verify real-model
quality and device flows. Cancelling local waiting must not promise that upstream
generation or charging was cancelled. This provider alone is not a shipped image
generation feature.
