# Journal voice v29: semantic prose and contextual components

Polish targets and output paragraphs carry a required semantic style. The small ordinary schema supports body, heading1/2/3, ordered/unordered lists, and checklist states, with at most 16 paragraphs and 6000 characters of output. Explicit numbered speech remains a list; a conclusion returns to body. Native numbering is rendered by the App. Existing target styles remain in model input across batches. Prompts retain complete facts and avoid adding emoji by default.

TableSourceContext and TimelineSourceContext carry up to 16 exact source excerpts each, bounded to 6000 characters. These excerpts are content evidence only. Creation commands must still come from pending speech; contextual sources cannot be consumed again or repartitioned as instructions. Shared source validation checks exact anchors, known identities and command/content separation. The App independently verifies the original recording and live document ownership.

Tests cover request construction, structured model input, semantic response decoding, cross-sentence table/timeline validation and rejection of fabricated or missing sources. The voice, development and journaldevserver packages pass race tests and vet. The diagnostic stream test now waits for handler completion before reading its log buffer, eliminating a test-only data race.

App and server must use journal-voice-v29 together. This source milestone does not deploy the running service, push branches, publish TestFlight or install a device build. Emotion UI is not restored.
