# Journal live voice protocol v32

`journal-voice-v32` separates audio quota acknowledgements from continuous
recognition. It is a breaking development protocol. A matching v32 App must be
released before this backend can be deployed alongside it; there is no fallback
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

The provider can send rolling windows, or an empty completed placeholder after
having returned audible provisional text for the same start time. That latter
case fails the current recognition with `speech_empty_utterance_after_text`
instead of erasing the provisional sentence or promoting it to final. The
client retains its last visible source and performs bounded replay from the
original audio. Structure-only diagnostics record empty-completed counts,
never transcript content, speaker IDs, audio or credentials.

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

Every emitted `polish` revision carries a server-generated UUID `id`. After
handling that result, the App persists its ID and returns it as the next
snapshot's `acknowledgedPolishID`, whether the text changed, the result was
already current, or a user edit caused the result to be skipped. Only that
exact ID advances the server queue. Pending source evidence is retained for a
fresh attempt. Paragraph equality is not an acknowledgement: an unchanged
context paragraph must not deadlock the remainder of a recording.

An empty rendered prose target is accepted only when it retains nonempty source
text and actual source turns. It can then recover from grounded speech. An
empty spacer without source evidence is invalid and must not be submitted.

For fully sourced targets, the model receives the original turns rather than
old generated prose as its mandatory baseline. Each turn's `narrativeRole` is
derived from the confirmed person and narrator (`author`, `other`, `unknown`).
All actions in one speaker's self-report retain that actor, including omitted
subjects in later clauses. A bounded, incomplete source window still retains
the previous prose so that facts outside the window cannot be discarded.

Ordinary one-person prose keeps its 2,048-token/25-second bound. Confirmed
multi-person prose uses low reasoning within 4,096 tokens and 45 seconds to
resolve reciprocal references and omitted subjects; stopping capture and local
audio persistence do not wait for this pass. These are bounds, not a promise
that every provider request completes within them. The explicitly enabled
synthetic semantic corpus preserves all failed attempts separately from App
capture acceptance.

Identity output cannot assign two contradictory names to one acoustic key or
change the narrator without an exact command span. Provider voice collisions
remain a known integration limitation: acoustic grouping is not proof that all
its turns belong to the same person. Further turn-specific identity application
is required before claiming the three-participant App journey accepted.

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
