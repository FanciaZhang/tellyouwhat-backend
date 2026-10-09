# Journal live voice protocol v32

`journal-voice-v32` separates audio quota acknowledgements from continuous
recognition. It is a breaking development protocol. The current v31 App must be
updated before this backend can be deployed alongside it; there is no fallback
that closes ASR at each audio checkpoint.

## Audio and recognition have different identities

- `segmentID` identifies one immutable PCM audio chunk, at most 480,000 bytes
  (15 seconds of PCM16 mono at 16 kHz). The App stores its audio original locally.
- `recognitionID` identifies one acoustic connection, shared by many chunks.
  Speaker numbers belong to this ID, never to the audio chunk or whole user.
- `startMilliseconds` is the recognition connection's offset in the original
  recording and must stay unchanged throughout that connection.
- `audio.final` ends only that chunk. It does not send the ASR final flag.
- `receipt` contains only `segmentID`, SHA-256 and the chunk duration. It confirms
  byte delivery and quota accounting, not transcript completion or backend audio
  storage. The App must not erase interim text when this ACK arrives.

The subscription lease and PCM hashes retain the previous billing invariants.
Each received checkpoint is committed independently. Duplicate billing is
prevented across reconnects. Replayed billed audio is buffered until its entire
hash is verified, then fed to the new recognition connection; an existing bill
cannot authorize changed bytes. A repeated chunk on the same connection does
not feed the provider twice. An ASR failure after an audio ACK does not discard
that ACK or make it a completed transcript.

## Transcript and explicit closing

`transcript` returns `recognitionID`, `startMilliseconds`, `milliseconds`, full
canonical text, and canonical utterances. `milliseconds` is the total duration
sent to that recognition connection; utterance/word times remain relative to
its start. Their identity does not change at an audio checkpoint. Clients add
the recognition offset for playback and recording-wide source provenance.
Provider completion status remains authoritative for each utterance; a
recording stop is not permission to invent a completed sentence.

After microphone stop, the App first sends all captured chunks and receives
all audio ACKs. It can send `capture_closed` to coalesce remaining editorial
work, then sends `recognition_finish` with the active recognition ID. This is
the sole signal that sends the provider's final audio flag. The resulting
`recognition_completed` has the same fields as a transcript event and is the
separate ASR completion acknowledgement.

Only after persisting that completion and sending the current document snapshot
may the App send `finish`. Premature `finish`, audio after recognition input
closes, and a changed recognition ID or offset during a connection are rejected.
Snapshot-only editorial work can finish without opening an ASR connection.
Document, source, narrator and identity acknowledgement fences continue to
apply independently of audio ACKs.

An interrupted connection requires a fresh recognition ID and a retained
recording offset. The App owns source reconciliation and must never equate
identical provider speaker numbers from two connections.

## Verification boundary

`continuous_session_test.go` covers multiple 15-second chunks on one recognition
connection, utterances spanning a chunk boundary, explicit ASR closure, verified
billed replay, duplicate input prevention, premature editorial finish rejection,
and retained audio ACKs after recognition failure. Existing revision/identity/
polish acknowledgement tests exercise the new explicit closing protocol.

`TestConfiguredContinuousVoiceProtocolAcrossAudioCheckpoints` is an explicitly
enabled synthetic real-service check through `Service.run`, with the real ASR,
identity and prose models plus rolling concurrency/cost control. It records
all events, verifies immutable audio hashes and quota, and requires speaker
turns, identity and prose before capture completes. Its success is backend
session evidence. It does not prove App paragraph projection, persistence,
manual editing, undo, reconnect reconciliation or UI acceptance.
