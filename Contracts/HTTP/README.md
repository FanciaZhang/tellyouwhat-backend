# Tellyouwhat HTTP contracts

`HealthAPI/openapi.yaml` and `JournalAPI/openapi.yaml` are the canonical public
gateway contracts. `AdminAPI/openapi.yaml` and `WorkerAPI/openapi.yaml` are
server-only contracts for the other HTTP runtimes. The contracts generate:

- the Gin router and Go wire models in the matching `internal/*httpapi`
  package; public App and worker runtimes also implement the generated strict
  server interface;
- the `HealthAPI` or `JournalAPI` Swift product at build time, imported directly
  by the corresponding iOS managed-AI transport;
- the future TypeScript Fetch client through the pinned OpenAPI Generator
  command documented below.

Do not edit generated Go or client code. Change the OpenAPI document, regenerate,
and let compile failures identify every server and client implementation that
must be updated.

## Go server

```sh
go generate ./internal/httpapi ./internal/journalhttpapi ./internal/adminhttpapi ./internal/workerhttpapi
```

## Swift client

```sh
swift build --package-path Contracts/HTTP
```

Swift OpenAPI Generator runs as a build plugin, so generated Swift code is not
committed and cannot drift from the contract. Each app calls its generated
`Client` for every gateway operation. App Attest, SSE, TOS, and background
`URLSession` code are platform adapters around serialized generated requests;
they must not define a second set of routes or JSON bodies.

## TypeScript Fetch client

The repository does not yet contain a TypeScript application. When one is
added, generate its client from the same document with OpenAPI Generator 7.24.0:

```sh
docker run --rm \
  -v "$PWD:/workspace" \
  openapitools/openapi-generator-cli:v7.24.0 generate \
  -i /workspace/Contracts/HTTP/HealthAPI/openapi.yaml \
  -g typescript-fetch \
  -o /workspace/Web/Generated/HealthAPI \
  --additional-properties=useSingleRequestParameter=true,supportsES6=true
```

Repeat the command with `JournalAPI/openapi.yaml` and a `JournalAPI` output
directory for Journal Web surfaces. Generated TypeScript belongs to the future
Web target and should be regenerated in CI. Do not introduce a second
hand-maintained Web schema.
