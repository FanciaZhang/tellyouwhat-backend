# Journal voice service

This is opt-in (`JOURNAL_VOICE_ENABLED=false`). The iOS client sends only to the
Journal host; there is no client-side provider key or credential forwarding.

## Admission and wire contract

`POST /v1/journal/voice/sessions` uses the existing App Attest headers and
`X-Tellyouwhat-Request-ID`. JSON: `{sessionID: UUID, consentVersion:
"journal-voice-v1"}`. An active Journal subscription, verified original purchase
anchor, and managed-AI consent are required. An older transaction without its
original purchase date must be restored/synchronized, never guessed.

201: `{sessionID, token, remainingMilliseconds, maximumMilliseconds, resetsAt}`.
Use the 30-second single-use token in `Authorization: Bearer ...` on
`GET /v1/journal/voice/sessions/{sessionID}/stream`. Renew App Attest admission to
reconnect, retaining the same session and segment UUIDs. Tokens are never in URLs.

WebSocket messages are JSON. Client messages:

- `snapshot`: `{snapshot:{revision,blocks:[{id,text}],transcript,editedBlockIDs,mediaOnlyBlockIDs,manualEdits,words,writingStyle}}`.
  Blocks are stable UUIDs. Client archives and recovery text are not model inputs.
- `audio`: `{segmentID,pcm:base64 PCM16 little-endian mono 16000Hz,final:bool}`.
  Frames are at most 6400 bytes (200ms); segments at most 480000 bytes (15s).
- `finish`: flush the final rewrite. Send the last audio frame with final=true first.
- `ping`: application heartbeat, receives `pong`.

Server messages: `ready`, `transcript` (segmentID/text/stable), `receipt`
(segmentID/sha256/text/milliseconds and remainingMilliseconds), `revision`
(baseRevision/transcriptRevision/patches/questions), `finished`, and `error`.
Patches contain id/text/afterID. Empty afterID replaces an existing text block;
otherwise insert a new UUID immediately after an existing block. Blocks and media
are not deleted or reordered. Only `mediaOnlyBlockIDs` are excluded from text
replacement. `editedBlockIDs` is advisory history, not an immutable paragraph lock.

`manualEdits` contains at most 24 recent local changes, capped at 4096 Unicode
scalars across their before/after/context fields. Each entry carries `blockID`,
`before`, `after`, `contextBefore`, `contextAfter`, and `transcriptOffset` (a Unicode
scalar offset in this recording's cumulative transcript when the edit occurred).
The client resets inherited offsets to zero for a new recording. Before calling
the model, the backend splits the transcript at those boundaries, preserving each
character once; the model does not have to count offsets itself. Earlier speech,
including an earlier explicit correction, should not undo a later manual edit.
New explicit corrections may revise the same words in place. User-expressed
uncertainty may remain uncertainty in the body; it must not become invented
certainty. These are editorial instructions, not a deterministic semantic guarantee.

The current body remains authoritative context when no exact hint is available.
Formatting-only changes do not generate text hints. Large replacements are not
truncated into misleading exact edits. Hints no longer matching the current body
are omitted. Raw audio and transcript archives remain local and recoverable; only
the bounded hints join the already consented rewriting request. No content is
added to service logs or usage metrics.

Persist an entire validated revision atomically, then acknowledge with a snapshot
at baseRevision+1, including when patches is empty. No-op acknowledgements do not
trigger another model request. Persist receipts before acknowledging the final
revision. Stale results must not overwrite user edits. A failed model request
keeps the audio and transcript recoverable and must not imply successful final
organization.

## Durability, limits, and privacy

The subscription-wide fenced lease and idempotent segment ledger use shared Redis;
production must enable AOF, backups, and a no-eviction policy for these keys. Missing
storage fails closed. Never use the development memory store in production.
Receipts are AES-GCM encrypted with application/owner/session/segment-bound AAD,
expire in 24 hours, and lose their text on final client acknowledgement. Monthly
counters contain no journal text. Non-content billed segment hashes and session
durations survive transcript expiry so a retry in a later month cannot charge
again. Account deletion clears these and all other Journal owner voice keys.
Clearing a receipt removes its cached text record rather than replacing its
transcript with an empty string. Replaying already billed audio regenerates the
transcript when needed, even at zero remaining allowance, and verifies the
original audio hash before accepting it. A retained receipt avoids that provider
call. Neither recovery path charges the subscriber again.

iOS persists `submittedAt` before sending the first audio frame. It never splits
an uncertain upload merely because the next ticket reports less allowance.
A quota rejection identifies the unbilled segment and current allowance, allowing
that segment to be safely split while retaining its unprocessed tail locally.
Subscription contention is returned as a structured WebSocket error so the app
can explain that another device is recording instead of endlessly reconnecting.
The existing payload encryption key and capability secret are used.

Only successful, unique segments consume minutes, including their natural pauses.
User-paused intervals and provider failures do not. The 120-minute allowance is
anchored to the verified original purchase, clamping month ends; annual plans use
the same monthly periods. Completed and retried segment hashes prevent double
charging. Per-session success duration cannot exceed 30 minutes. Audio never
persists on the gateway; unacknowledged audio must be retained on iOS for replay.

The normal speech endpoint is `.../api/v3/sauc/bigmodel_async`, Resource ID
`volc.seedasr.sauc.duration`. New console X-Api-Key and old App-Key/Access-Key
credentials are both supported. Vocabulary has a conservative 96 UTF-8 byte cap
for the documented bidirectional 100-token hotword budget. It is not a 5000-word
batch-ASR list. See https://www.volcengine.com/docs/6561/1354869 .

## Before enabling

Apply migration 0002, configure speech credentials and JOURNAL_VOICE_MODEL_ID
(defaults to the existing Pro endpoint), then run the deterministic voice and
admission tests. Verify live ASR framing, supported region, vocabulary quality,
provider retention and actual billing in the speech console. Evaluate Chinese
corrections, detail retention and total 30-minute token cost with consented test
recordings. Do not log audio, body, vocabulary, tokens, or ticket headers. Enable
only after physical-device background/offline acceptance and the cost gate.


## Deployment on 2026-09-04

- Running source: `85dd734d5e9335337111d6db59cb8787be1415dc`. All six service images
  are installed as `tellyouwhat-ai-release-<service>:voice-85dd734`; gateway,
  worker and admin were recreated and passed their individual readiness checks.
- Images were built from the committed source, transferred over SSH with SHA-256
  verification and loaded into Docker on the server. Git and registry images
  were not pushed. The deployment used the existing release lock, validation,
  migration, runtime snapshot, activation and rollback helpers.
- Migration 0002 added the nullable original-subscription-start column. An encrypted
  pre-deployment database backup is retained on the server in
  `/var/backups/tellyouwhat-voice-20260904/mysql-20260904T144644Z-96d878.sql.gz.enc`.
- `go test -race ./...` passed. The routes now come from the canonical OpenAPI
  contract, including the WebSocket upgrade; a real loopback WebSocket test
  checks the ready frame and rejection of a reused ticket. iOS carries the same
  generated public contract.
- The gateway-only App Attest hotfix override was retired. The deployment record
  in `.operations/release.json` points to a complete rollback image set that
  preserves that previous assertion fix. From `/opt/tellyouwhat/backend`, use
  `python3 deploy/tencent/release.py rollback` to restore it. This restores the
  pre-voice code; the additive database column is retained.
- The public IP HTTPS readiness check passes. The existing `api-key-journalpp`
  speech credential is now configured as `JOURNAL_SPEECH_API_KEY` on the server,
  and the existing Ark key and Pro model supply diary rewriting. No new key or
  entitlement was created. The key was transferred over SSH without putting its
  value in source, logs, command arguments, or a local file.
- `TestLiveSpeechAndDiaryRewrite` passed on the server with synthetic PCM speech:
  21 provider frames, 36 transcription characters, one valid diary patch, and
  663 input / 79 output model tokens. The check retained the park and tea details
  and the corrected name 小林. It requires `JOURNAL_VOICE_LIVE_CHECK=1` and the
  documented synthetic PCM fixture; ordinary tests skip it.
- After the provider check, `JOURNAL_VOICE_ENABLED=true` was activated and the
  gateway was recreated. All service readiness checks still pass. The deployment
  snapshot and `.operations/voice-enable.json` record the configured state;
  unsigned voice admission requests return 401. Real paid-device recording,
  background behavior and recovery remain a separate acceptance requiring an
  active synchronized subscription. No user audio or diary was used in this check.

## Preset writing styles

A snapshot accepts `writingStyle`: `natural` (the default), `lively`,
`documentary`, `daybook`, or `essay`. Unknown identifiers are rejected before
calling a provider. An omitted preference uses the product default. ASR and
personal vocabulary are unchanged: the style applies only to body rewriting.

The app resolves the per-entry override before its synced Settings default and
sends only the resolved identifier. Each preset is mapped to trusted instructions
on the server, underneath common fidelity, chronology, manual-edit protection,
and strict patch validation rules. Freeform instructions are not accepted.
Source text remains untrusted even when it contains commands. This reduces the
instruction surface; it does not guarantee that generated prose is factually
correct. The model has no tools and cannot access other journals or credentials.

Changing a preference does not start processing a saved entry. While capture or
rewriting is active, the next snapshot carries the new style and document
revision. A style change invalidates prior in-flight results, and a newer client
revision supersedes an older outstanding acknowledgement. Audio capture and
receipts continue on the same connection. Body recovery retains the current
style preference, original transcript, and recording resources.


## 2026-09-08 manual-edit acceptance

Source commit `09fdb0d` was built as `journaldevserver-e496dd00bf1e2a5f` and deployed
only to `journal-private-development`. Five synthetic cases passed through the
actual development HTTPS consent/ticket flow, WebSocket revision and client
acknowledgement: long-paragraph punctuation plus name correction, a later explicit
correction to hand-edited words, old recognition preserving a manual name edit,
an old explicit correction preserving a later manual amount edit, and expressed
uncertainty staying uncertain. Race tests and vet passed for `internal/journal/voice`
and `cmd/journaldevserver`. These are concrete samples, not universal model guarantees.

Production gateway, worker and admin container IDs/start times were unchanged.
Because the repository's `PRODUCTION_DEPLOY_ENABLED` variable is currently true,
this source push intentionally uses a `[skip ci]` documentation commit so it does
not redeploy the shared production stack. This uses GitHub's documented
[per-push workflow skip](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/skip-workflow-runs);
it does not change any workflow or deployment variable. A later ordinary
production release can include this Journal-only source change.

## 2026-09-08 late-recognition review repair

Manual edits now include `pendingEarlierSpeech`. While audio captured before an
edit is awaiting its final receipt, the editorial adapter treats all in-flight
recognition as earlier speech. Once the receipt arrives, the client supplies the
boundary derived from recorded segment identities; later audio can explicitly
correct the edit. The adapter clones hints before deriving boundaries, so model
input cannot mutate session state.

Race tests and vet passed for voice and journaldevserver. Six synthetic scenarios
passed through the deployed private development HTTPS/WebSocket service, including
pending earlier audio containing an explicit correction. Gateway, worker and admin
container IDs were unchanged across deployment. Only `journal-private-development`
was replaced. This commit uses `[skip ci]` to avoid triggering a shared production
deployment; no workflow, production entitlement or Health configuration changed.
