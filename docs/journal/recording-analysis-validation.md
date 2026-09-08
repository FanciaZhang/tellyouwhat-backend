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
