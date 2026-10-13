# Journal-owned model instructions

Journal App clients submit typed business material. Journal backend owns model
instructions, provider parameters, schemas and generation defaults. Health and
Birthday contracts and business behavior are outside this change.

| Endpoint | Responsibility | Instruction owner |
| --- | --- | --- |
| `POST /v1/journal/organization` | Tags and book recommendations | Published Journal organize policy |
| `POST /v1/journal/illustration-suggestions` | Optional scene grounded in body text | Journal provider and shared scene contract |
| `POST /v1/journal/illustrations` | Image job from a confirmed brief | `internal/journal/illustration/brief.go` |
| Journal voice session API | Existing voice workflow | Existing Journal voice policy and schemas |

Organization no longer includes an illustration switch or illustration result.
The scene endpoint has no books, caller instructions or model selector. Internally
the two text operations reuse authentication, consent, entitlement, quota and usage
enforcement under the existing Journal organize accounting budget; this is not a
public generic AI endpoint. Voice behavior and protocol version are unchanged.

Image admission validates `brief.title` and typed `brief.details`, compiles provider
instructions once on the server and freezes them in the encrypted job for retries.
Unknown caller `prompt`, `instructions` and `model` fields are rejected. Scene and
image templates are source-owned server instructions; this change does not add
admin editing controls for them.

The canonical API is `Contracts/HTTP/JournalAPI/app.openapi.yaml`. Regenerate with
`go generate ./internal/journalhttpapi` and synchronize the resulting Journal
OpenAPI snapshot to the App. Removed routes/fields have no compatibility aliases;
App and backend releases must be coordinated. Deployment and enabling a previously
unavailable production image service are separate operations.

## Verification on 2026-10-13

Journal, gateway, prompt configuration/evaluation, generated contracts, Health
contract isolation and OpenAPI composition tests passed. `go vet` passed for
Journal, gateway and the generated Journal API. Dedicated-route tests verify
consent enforcement and that the removed route cannot invoke a provider.

Real synthetic provider checks produced a grounded scene and a 2496 × 1664 image;
App checks retained their exact returned content through insertion, persistence and
reopening. The first scene attempt timed out and is retained. Organization quality
checks completed ten cases: nine raw outputs passed, one Lite output suggested a
duplicate book which existing curation removed. Do not report this live quality
suite as entirely passing. All ten curated results passed App tag/book persistence
checks. No new policy was published during this refactor.

Evidence is retained outside source control in
`~/Documents/JournalAppAcceptance/server-prompts-20261013/`.
