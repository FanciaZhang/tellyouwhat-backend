# Whole-recording analysis capability probe — 2026-09-08

This is an isolated provider-contract milestone, not a completed app feature.
No production route or existing streaming behavior changes in this milestone.

## Real provider observation

- Provider: standard file recognition, `volc.seedasr.auc`.
- Endpoint: `/api/v3/auc/bigmodel/submit`, then `/query`.
- Flags: `enable_speaker_info: true`, `enable_emotion_detection: true`,
  `show_utterances: true`. Whole WAV supplied via `audio.data`.
- Input: 22.649 seconds, PCM16/16 kHz/mono, two macOS synthesized Mandarin
  voices alternating four turns about a hiking trip. No user audio used.
- Submit: HTTP 200, provider status `20000000`, 2.22 s.
- First query: HTTP 200, completed at 5.42 s from start.
- Speaker sequence: `1, 2, 1, 2`. Both identities were consistent across the
  15-second boundary used by the app's live-transcription transport.
- Utterance ranges: 80–5960, 6480–11680, 12350–16470, 16990–22030 ms.
- Each acoustic emotion was the literal provider string `neutral`.
  Spoken content explicitly mentioned fear in the past and happiness now;
  acoustic labels must not replace or reinterpret those stated feelings.
- Submit log ID: `20260908212523F7FA727B8653BD7F1BA2`.
- Query log ID: `202609082125285A2FA417CCE88678D3AE`.

The committed fixture retains only text, ranges, speaker and original emotion;
provider scores and word-level details are intentionally not product data.

## What this does not prove

Neutral synthesized voices do not validate emotion accuracy, overlapping real
voices, four-to-six participants, or device recording quality. These acceptance
items remain open. The API response contains duration, not actual account cost;
charged amount still requires the provider billing record. Do not label a local
price estimate an actual invoice or mark these gates passed.

The turbo endpoint is excluded: the official description explicitly removes
emotion detection fields. See https://www.volcengine.com/docs/6561/1631584 and
standard reference https://www.volcengine.com/docs/6561/1354868 .

## Repeatability and safety

`TestLiveRecordingAnalysis` is opt-in via `JOURNAL_LIVE_RECORDING=1` and
`JOURNAL_LIVE_RECORDING_WAV` (canonical 44-byte-header WAV). Provider credentials
come from the private Journal service environment, never committed files.
One invocation submits one task; recorded IDs must be queried after uncertain
submission, not replaced by a new task ID. Ordinary tests use the saved synthetic
response and an in-process HTTP server. Tests cover bounds, future emotion text,
unknown speakers, stable utterance IDs, task separation, and credential redirects.

## Six-voice probe: accuracy gate failed

A second 56.392-second synthetic recording used Tingting, Meijia, Eddy, Flo,
Grandpa and Shelley (Mandarin variants), twelve turns with each voice recurring.
It completed 13.15 seconds after submission began (submit accepted at 6.71 s;
first poll at 9.91 s pending). Actual output contained only three speaker labels
and nine utterances. Multiple adjacent source speakers were merged into single
utterances. Labels also changed for a recurring voice.

- Submit log ID: `202609082129545CACBB248481046CB9CC`.
- Final query log ID: `2026090821300700CC8E62AF9BCD7CFE86`.
- Fixture: `recording_six_synthetic_voices.json`.

This test is not evidence that six real people will always fail (some system
voices may share a synthesis base), but it definitively does not pass the
planned six-speaker acceptance. Correction needs both splitting incorrectly
merged speakers and splitting an utterance spanning more than one real voice;
merging over-segmented identities alone is insufficient. A dedicated real-voice
corpus is required before product accuracy can be accepted. Do not derive missing
identities from the input text or manufacture six labels to satisfy the test.

All returned emotion strings were again `neutral`. Emotion field availability
is proven; emotion usefulness/accuracy remains unverified. Both probes submitted
79.041 seconds in total. This is measured provider duration, not confirmed billed
amount. No private Journal deployment or Health production restart was performed.

## Authorized real recording, 2026-09-09: long-input transport

A user explicitly supplied one approximately 15-minute two-person recording for
this evaluation. Neither audio, transcript, inferred identities nor rewrite
content belongs in this repository. Private evaluation files stay outside Git.

The original 60-second submit deadline failed with an uncertain result. Querying
the SAME task returned `cannot find task`; the next attempt retained that task ID.
With a three-minute HTTP deadline and streamed base64 (instead of a second full
JSON copy in memory), the task completed in 192.8 seconds, returning 217 timed
utterances for 912128 milliseconds of decoded audio. This is end-to-end time,
not pure inference time; phase-level timing was added for future probes.

The measured memory improvement is structural: no full base64 JSON allocation
is made by Submit. Peak process RSS has not yet been measured. The old input
buffer is still present and further end-to-end upload/storage design is required.

Live harness improvements: explicit stable task ID, query-only recovery, longer
poll deadline, and a private output-file option. Remote temporary audio and
results were removed after retrieval. No production service deployment occurred.

## Source fidelity and channel probes

The real two-person sample yielded two clusters with lexical speech and a third
cluster containing only eight acknowledgements/laughs. SpeakerEvidence now keeps
that evidence without treating the extra cluster as a confirmed third identity.
All source IDs remain unchanged. An entire-recording diarization accuracy score
is still unavailable without a manually timed reference.

Independent on-device Whisper transcription found a short supplementary fact
missing from the full ASR result; the user explicitly confirmed it. Four eight-
second probes then compared downmix, left, right and preserved stereo. Only the
left-channel probe recovered the supplement. Merely preserving stereo in a
standard request did NOT solve it. The recovered text still shared one speaker
label with the other voice, so blindly merging local speaker IDs would be wrong.
Canonical WAV validation now preserves one/two channels and counts duration by
frames, not channel samples. No automatic source correction is claimed.

Three rewrite variants were compared on the same private transcription. An eight-
item source-fidelity regression checklist scored 2/8 for the original rewriter,
5/8 with speaker context, and 7/8 after tightening attribution and uncertainty
instructions. This is a single-sample, post-discovery checklist, NOT a general
accuracy benchmark. Lost numeric uncertainty remains a failure, and another
activity was mislabeled as an examination. A narrow review helper detects numeric
qualifier loss without silently rewriting it; it is not an exhaustive semantic
validator or an app-level apply gate.

The LLM ignored dialogue formatting, so dialogue rendering now operates directly
on the validated utterances. All 217 utterance IDs/texts were preserved in order
in 66 consecutive speaker turns. Unmapped labels remain unconfirmed. Narration
still uses the rewriter; dialogue callers must use RenderRecordingDialogue,
not ArkRewriter. App integration/confirmation/preview remains to be implemented.

Removing provider bookkeeping from the model input reduced input tokens from
26722 to 13861 in this sample (about 48%). The second narrative took 43.2 s versus
46.9 s previously; one run is not enough to claim a latency improvement. Emotion
strings are retained verbatim and never interpreted as the event-time feelings.

Private output files, the user's text, case facts and sample-specific rubric are
not committed. Only synthetic pattern tests and general implementation are stored
in Git. No Health production or private Journal service deployment was performed.

## Durable execution foundation, 2026-09-10 (not yet an HTTP/App feature)

`RecordingExecutor.Process` performs one operation on a persisted recording job.
Only an uploaded job reserves provider budget and submits audio. Submitting or
processing jobs query the saved provider ID, including after a lost submission
response or server restart. Repeated completion reads do not call the provider.
Concurrent calls for the same owner/job are rejected, and the store revision
rejects a completion arriving after another operation has failed/cancelled it.
Invalid result identities/ranges fail without inventing replacement data.

Provider budget admission uses the existing cost controller. This does not debit
the user's streaming-minute quota. Reservations are conservative estimates, not
confirmed invoices. A budget denial leaves the upload available for later retry.
After an uncertain submission the executor only queries: a missing-task response
is not yet classified as safe resubmission. Scheduling, authenticated HTTP routes,
expiry maintenance, explicit retry policy and App integration remain pending.

Focused deterministic tests cover lost-response/restart recovery, exhausted
budget, repeated completion, concurrent calls, late results and invalid provider
identity; the executor tests also pass the Go race detector. No new deployment
was performed for this foundation.
