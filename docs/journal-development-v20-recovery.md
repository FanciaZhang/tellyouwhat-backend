# Private Journal voice deployment recovery — 2026-09-24

The production release did not update the independently mounted private development executable. Its container image identifies the runtime base, not the mounted Go program's revision. Do not infer the voice protocol from that image tag.

Authenticated synthetic admission reproduced the failure: privacy consent returned 200, but a v20 voice-session request returned 422 `voice_consent_required`. Updated admission distinguishes missing consent from `voice_protocol_mismatch` (409). The private API retains `subscription-v2`, required by the current App's passage-provenance contract.

The first update could not replay an existing budget-change event. Restored the previously implemented private budget-history support from `7c5b5e4`, retaining current recording support and all cost records. No ledger reset or credential rotation was performed. The service then started with zero restarts and v20 admission returned 201.

Synthetic live-model acceptance also exposed optional emotions inferred from words without acoustic evidence. The incremental model boundary now discards these unsupported decorations, preserving ordinary prose for full revision/source validation. It does not relax text-edit or provenance validation.

Deployed private executable source: `561524f`. Production gateway, worker and admin container IDs and start times stayed unchanged during private-service updates. Health and Journal public readiness checks passed. This recovery does not constitute physical-device audio acceptance, and the private-only fixes have not been deployed to the shared production gateway.

Verification: focused gateway/development/server/voice tests passed; explicit synthetic provider tests reproduced the unsupported-emotion failure. Public authenticated v20 admission passed after deployment. The final deployed build passed the synthetic public WebSocket journey: authenticated admission, ready, real-model structured revision, consumed-source acknowledgment, finished. No private journal or user audio was used.
