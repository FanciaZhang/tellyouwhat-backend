# Journal live voice protocol v33

`journal-voice-v33` adds grounded source-level person assignments to the
independent audio/recognition lifecycle introduced in v32. It is a breaking
development protocol. The matching local App now consumes these scopes and
source-selected narrators. Actual App attempt 8 with synthetic three-person
audio passes capture, inferred people, spoken author correction, prose application,
complete audio preservation, and controller reopening with real AI name correction.
This does not prove disk reopening, restored paragraph labels or user acceptance.
This source has not
been deployed. No compatibility decoder or acoustic checkpoint fallback is provided.

## Person assignments have an explicit scope

Every identity assignment contains `speakerKey`, `scope`, `sourceIDs`, `name`,
`personID`, `kind`, and `evidenceIDs`. `scope: voice` establishes a voice default
and must list every turn from that voice in the request. `scope: sources` only
assigns the listed original turns. Multiple people can share one acoustic key
when the provider merges their voices; their source assignments must be
disjoint. A conflicting introduction does not rename the earlier participant.

Grounded introduction and later connected experiences may be assigned together.
Their evidence IDs and target source IDs have distinct roles: an introduction
supports the person's name; each listed source identifies actual speech to
attribute. Source IDs must belong to that acoustic key and may not appear twice.
A voice-wide assignment cannot erase an already mixed set of known people.
`explicitSourceIDs` identifies manually claimed turns; ordinary inference may
not overwrite these or an explicit voice assignment. Existing person IDs can
be quoted from speaker defaults or source turns, with no invented IDs or merge
based solely on an equal name.

`unreliableSpeakerKeys` carries acoustic collisions already detected by the App.
Once one key merges different known people, defaults for the whole recognition
are unreliable. New grounded claims use `scope: sources`; ordinary inference
cannot reestablish a voice default from another numeric key in that connection.
Existing source claims remain intact, and subsequent ambiguous speech stays unknown.

App attempt 7 exposed a source mapping bug despite initially correct model
`targetIDs`: atomic paragraph reordering assigned every output to every input
source. The client now maps each output's actual target dependencies independently
of atomic application order. A source split into several prose paragraphs remains
evidence for all its paragraphs. Attempt 8 uses the exact same validated synthetic
PCM and avoids the prior duplicate travel and unsupported pronouns. Earlier failed
attempts and captured request chains remain in the acceptance artifact directory.

A real configured-model replay of the failed synthetic App transcript now
separates 小林's introduction and pastry-purchase experience from 老公宝's
speech, without changing the captured text or inventing a third acoustic key.
This replay proves model output and validation only. The App now freezes existing
paragraph identities before clearing a conflicting voice default, so future
unresolved turns do not inherit either participant. Complete real capture still
has to verify those application paths.

Identity requests carry `narratorPersonID`, independently of acoustic keys.
An explicit viewpoint operation returns `narratorSourceID`: a real original turn
belonging to the selected person, including a person sharing a voice key with
someone else. `narratorEvidenceIDs` and exact `commands` spans ground the operation;
the operation issuer does not automatically become its target. An unchanged,
known-person assignment echo is omitted after references and scope are validated;
an actual name change still needs literal evidence.

The first configured-AI corpus run passed six of seven cases; a redundant default
"我" echo with no literal name blocked an independently correct introduction.
After the no-op correction, all seven cases pass, including merged-voice narrator
selection. Focused checks, race checks and vet pass. This is model/protocol
evidence and is not full App or user acceptance.

The next real App attempt reached source identity and spoken narrator selection,
but repeated introduction confirmation created a duplicate person, and identity
progress did not extend the finishing inactivity clock. Its full attempt remains
failed. Replaying that exact synthetic App request with the configured model also
showed that a later anonymous packing remark shared only a pastry topic/acoustic
key with the third person. The tightened inference leaves this turn unknown while
retaining the third person's introduction and purchase; the real replay and
backend race checks pass. It does not claim that ASR separated three acoustic voices.

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
multi-person prose uses 4,096 output tokens and at most 90 seconds. Person
inference resolves grounded identities before prose; explicit `attributionKey`
and `narrativeRole` let the prose pass follow those identities with reasoning
disabled. Unknown turns have distinct unresolved keys, never a shared person
inferred from their acoustic number. Stopping capture and local
audio persistence do not wait for this pass. These are bounds, not a promise
that every provider request completes within them. The explicitly enabled
synthetic semantic corpus preserves all failed attempts separately from App
capture acceptance.

The fourth real three-person App attempt preserved three source identities
within two actual acoustic keys, including an unresolved later packing remark.
Its author correction request nevertheless timed out after 45,002 milliseconds,
at the same instant as the App's former 45-second inactivity guard. The larger
multi-person request bound is paired with a bounded App retry window; configured
lower timeouts still take precedence. It does not slow local recording stop or
save. The same synthetic capture is being rerun; this timing change alone is not
complete App acceptance.

The fifth capture completed prose but failed complete acceptance. Its synthetic
name was misrecognized, and manual review found invented gender and an unknown
speaker's action assigned to the narrator. Captured-request replays retained
these failures. Low reasoning used roughly 54 seconds for one request and
returned no prose after about 74 seconds for another; direct constrained prose
then completed in roughly 11 seconds. Unknown first-person claims must remain
subjectless or quoted, rather than silently becoming the narrator. A targeted
single-actor self-reference normalization protects names without modifying
mixed actors, source-supported pronouns or quotations. These replays are
model evidence; the sixth full App attempt is still being verified.

Identity output cannot assign two people to overlapping source turns or change
the narrator without an exact command span. Provider voice collisions are preserved
as actual acoustic keys; grouping is not proof that all turns belong to one person.
Source-specific assignment provides the representation, but real App evidence is
required before claiming the three-participant journey accepted.

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

## v33 完整原话使用逐段人物输入（2026-10-10）

连续 App 第 14 次人工复核发现医院值班句省略主语；实际输入中人物已正确，但完整来源又被拼成普通文本基线，后续整理承接了共同经历的主语。preparePolish 对有完整 turns 的目标移除旧正文基线及无人物的拼接原话，只用逐段原话及其 personID/narrativeRole/proseSubject；不完整来源仍保留已有事实。换人描述个人经历时先写明称呼。

两个确切失败请求重放通过；未知来源旧请求重放通过。两轮提示长度检查失败均保留，精简后回到 9500 字节小任务约束；一个语义检查误拒绝“老婆宝…她”的明确指代，修正后再请求真实模型通过，原先省略主语错误依然被拒绝。voice/development/server 竞态检查与 vet 通过。

第 15 次完整 App 真实 ASR/AI 连续录音通过：138.307 秒，52 秒主动断线、92 秒录音中补记，2 条实际连接、20 个原话段落、零已显示事实消失、人物经历/音频/补记保持，停止调用约 26ms。单人实际录音复测亦通过，48.417 秒、4 个原话段落、无人物标签、4 段自然正文。语音功能组 324 通过、10 未启用联调跳过、0 失败。证据位于 JournalAppAcceptance/20261010-continuous-app 和 20261010-single-speaker-app；没有生产或 TestFlight 发布，用户验收待完成。
