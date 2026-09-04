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

- `snapshot`: `{snapshot:{revision,blocks:[{id,text}],transcript,editedBlockIDs,words}}`.
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
are not deleted or reordered. User-edited blocks cannot be replaced.

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
