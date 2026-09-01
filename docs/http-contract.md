# HTTP contract architecture

## Decision

The backend is OpenAPI-first and uses Gin as its HTTP runtime. Framework and
contract responsibilities stay separate:

- OpenAPI owns paths, methods, operation IDs, wire models, status codes, and
  authentication header declarations.
- `oapi-codegen` generates the Gin router and Go wire types for every HTTP
  runtime. One shared Platform handler and each App-specific handler implement
  their generated strict server interfaces. The administration service implements the generated Gin server
  interface directly because WebAuthn verification consumes the original HTTP
  credential request.
- Swift OpenAPI Generator generates the Apple client and types at build time.
- OpenAPI Generator generates a TypeScript Fetch client when a Web target is
  introduced.
- Gin owns route dispatch and middleware composition. It is not a second source
  of route definitions.

Hertz remains suitable for Go services that standardize on CloudWeGo. Its `hz`
tool generates Hertz Go routers, models, and Go clients from Thrift or Protobuf,
but it does not by itself generate the Health Swift and TypeScript HTTP clients.
Using it here would require another cross-language contract pipeline while also
reimplementing the App Attest and SSE adapters already built around the shared
OpenAPI runtime.

## Contract boundaries

`Contracts/HTTP/PlatformAPI/openapi.yaml` owns shared public paths, schemas,
security schemes, and the vendor-wide `X-Tellyouwhat-*` headers.
`Contracts/HTTP/HealthAPI/app.openapi.yaml` and
`Contracts/HTTP/JournalAPI/app.openapi.yaml` own only product-specific paths and
components. The self-contained `HealthAPI/openapi.yaml` and
`JournalAPI/openapi.yaml` files are generated client artifacts, not additional
sources of truth.
`Contracts/HTTP/AdminAPI/openapi.yaml` and
`Contracts/HTTP/WorkerAPI/openapi.yaml` are isolated internal contracts; they
are never included in App clients.
`deploy/schema-manifest.json` is the managed-AI business contract. The
manifest binds operation and prompt versions to JSON Schema digests and model
policies. They are intentionally separate and must not duplicate each other.

The Apple notification endpoint is part of the server contract but is not used
by generated app clients. The background job upload can use generated models,
but its final upload remains on a background `URLSession` because iOS owns that
lifecycle.

## Security and streaming

App Attest signs the exact HTTP method, escaped path, request ID, nonce,
timestamp, and raw body SHA-256. Gin captures and restores those bytes before
the generated strict handler decodes them. The business layer receives generated
request objects while authentication always hashes the actual transmitted body.

OpenAPI 3.1 identifies the AI stream as `text/event-stream`, but does not express
the three named event payloads as a portable discriminated stream. The generated
client owns the request and response body type; the small `delta`, `completed`,
and `error` event decoder remains handwritten and tested until the project
adopts a generator with compatible typed-SSE output.

## Adding an endpoint

1. Add a shared operation to PlatformAPI or a product operation to the matching
   App IDL with a stable
   `operationId`, closed request/response schemas, and explicit error statuses.
2. Run `make generate-api` in `Backend`.
3. Implement the newly required generated Go server method. Public App and
   worker operations return only generated strict response objects; Admin
   operations are native Gin methods registered only by generated code.
4. Build the Swift contract package and regenerate the Web client when present.
5. Add contract, handler, and client adapter tests.
6. Run `make verify-generated`, `go test ./...`, `go test -race ./...`, and
   `go vet ./...`.

CI rejects stale generated Go code and breaking OpenAPI changes. A deliberate
breaking change requires a new versioned path or a coordinated client release;
the check must not be bypassed by maintaining an alternate schema.

## Multiple apps

Each App gets a separately named, composed public OpenAPI document and generated
client package. The Host mux selects one App before authentication, then that
App's Gin engine registers the generated Platform router and exactly one
generated product router. Reusing `/v1/attest/*`, `/v1/privacy/*`, and other
platform paths across domains is intentional; their contract and implementation
remain singular. Adding an App costs one product IDL and handler rather than a
copy of every platform endpoint.

Every gateway access log includes `app_id`, `host`, `operation_id`, and
`request_id`, so identical paths on different domains remain distinguishable.
Admin and worker keep separately named, server-only OpenAPI documents, and no
public App client receives their operations or credentials.
