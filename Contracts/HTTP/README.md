# Tellyouwhat HTTP contracts

`PlatformAPI/openapi.yaml` is the canonical contract for capabilities shared
by every App. `HealthAPI/app.openapi.yaml`, `JournalAPI/app.openapi.yaml` and `BirthdayAPI/app.openapi.yaml`
contain only App-specific operations. The `openapi-compose` build tool merges
those sources into the generated, self-contained `HealthAPI/openapi.yaml` and
`JournalAPI/openapi.yaml` and `BirthdayAPI/openapi.yaml` public client contracts. Never edit these bundles
directly.

`AdminAPI/openapi.yaml` and `WorkerAPI/openapi.yaml` are server-only contracts
for the other HTTP runtimes. The contracts generate:

- the Gin router and Go wire models in the matching `internal/*httpapi`
  package; public App and worker runtimes also implement the generated strict
  server interface;
- the `HealthAPI` or `JournalAPI` Swift product at build time, imported directly
  by the corresponding iOS managed-AI transport;
- the future TypeScript Fetch client through the pinned OpenAPI Generator
  command documented below.

Do not edit generated Go, generated public bundles, or client code. Change the
Platform or App source IDL, regenerate, and let compile failures identify every
server and client implementation that must be updated. Component and path
collisions fail composition instead of silently overriding a definition.

## Go server

```sh
make generate-api
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

## Album storage client

`AlbumAPI/openapi.yaml` is the canonical album account-storage contract. The
`AlbumAPI` Swift product is generated from it by the same pinned build plugin.
`AlbumAPITransport` supplies the exact serialized control request to an
app-owned sender; the sender must supply verified account authentication and
bounded responses. It must not substitute an App Attest device ID for an account.
Media bytes are uploaded separately through file-backed transfer tasks using
server-issued object grants. No production album endpoint is embedded in this
package.

Album resource manifests optionally carry `descriptor` with the original
PhotoKit resource type, filename, and uniform type identifier. New iOS uploads
send descriptors for every resource, including opaque companions. The server
validates type/role agreement and requires all-or-none descriptors; they are
part of the immutable manifest digest and persisted JSON. Filenames remain
untrusted descriptive metadata, never object keys or filesystem destinations.
Descriptor-less legacy manifests retain their exact digest/serialization for
idempotent recovery. They must not be treated as proof of full PhotoKit restore
capability. Optional manifest `metadata` additionally preserves capture time (UTC integer
milliseconds), dimensions, duration, media subtype flags, favorite, and valid
location/altitude. It participates in the immutable digest; absent metadata
keeps legacy identities unchanged. These snapshots do not yet implement an
editable cloud index, album membership, or a restore endpoint. Deploy this accepting backend before
clients start sending descriptors; older strict decoders reject the new field.

Verified immutable album versions are available at
`GET /v1/albums/library/versions?after=<sequence>&limit=<1...100>` and
`GET /v1/albums/library/versions/{versionID}`. The authenticated account supplies
ownership; query/body owner injection is rejected. Pages use publication order,
not capture-date gallery order, and may be shorter than the requested count to
keep JSON below 1 MiB. Advance only to `nextSequence`; `hasMore` covers both
count and byte truncation. Provider locations are omitted. Versions require
subsequent logical-asset grouping, deletion/change handling and scoped media
read authorization before they form the complete cloud gallery experience.


Explicit manual content-category choices use
`GET`/`PUT /v1/albums/library/versions/{versionID}/content-correction` and
`GET /v1/albums/content-corrections?after=<sequence>&limit=<1...100>`.
The session supplies ownership. PUT requires an immutable manifest digest,
request UUID, expected revision, and disjoint `included`/`excluded` category
arrays; no OCR, inferred labels, media, or client owner is accepted. A missing
record reads as revision/sequence zero. Empty arrays persist an explicit reset.
The current identical request replays without another revision; stale or changed
requests return 409. Clients must resolve conflicts explicitly rather than retry
with an updated revision and silently overwrite another device.

The feed contains each version's latest explicit state, not edit history. Follow
`nextSequence`; an edit during pagination may return a version again. Account
locking orders allocation and commit, so committed edits cannot appear behind
an already-returned cursor. Trashed-version choices remain in the owner feed
for restoration, but per-version access requires visibility. Join with the
current authorized catalogue before display, and process its purge events;
permanent version removal cascades the correction row. Retention, account
removal, and recovery quarantine fence these endpoints. This transport does not
implement the iOS sync queue, user consent, or cross-device UI.


`POST /v1/albums/library/versions/{versionID}/resources/{resourceID}/read-grant`
is bodyless and accepts no query. It authorizes the authenticated account's
published immutable resource for five minutes. The response binds asset,
manifest digest, version, resource, provider object version, byte count and
SHA-256 to a signed COS GET. The app must validate those bindings, its configured
storage host allowlist, the version query/response and downloaded bytes before
restoring. Do not log or persist the signed URL. It is not a public share link,
a download-count limit, immediate revocation mechanism, or deletion proof.


Custom cloud collections use `GET /v1/albums/collections` and
`GET /v1/albums/collection-members`, each with its own `after`/`limit` cursor.
`GET`/`PUT /v1/albums/collections/{collectionID}` reads or changes a name or
persists a deletion tombstone; `GET`/`PUT` on its `/members/{assetID}` changes
one logical asset membership. Members must reference the same owner's active,
verified backups. Removing a member or deleting a collection never deletes media.
System PhotoKit albums remain a separate local/Apple-owned scope.

Writes require an immutable request UUID and observed revision. Only the latest
identical request replays; stale requests conflict. A deleted collection cannot
be revived by a late rename or member request; recreate with a new UUID. Names
are trimmed, contain no control characters and have 1–80 Unicode characters.
Deletion requires an empty name. Explicit member removals remain in the feed;
join both feeds with active collections and the authorized catalogue, including
its asset purge events. A pagination-time edit can return the same entity again.
Commit each feed page and its cursor atomically on the client.

The endpoints reuse account locking, library rate limits, export-period access,
quarantine and deletion fences. They do not grant media access or upload quota.
Both tables participate in authenticated encrypted snapshots and account erasure;
permanent asset erasure clears its memberships but retains other grouping data.
Native durable sync/UI and the existing unfinished automatic retention scheduler
(including collection metadata expiry) still need integration before enabling the
complete feature. These backend operations alone do not establish reinstall or
cross-device acceptance.
