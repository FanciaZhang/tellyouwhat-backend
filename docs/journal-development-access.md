# Journal development cloud access

Local DEBUG subscription access and backend entitlement are separate. A local
switch never proves that authenticated voice or organization requests are allowed.
Keep `APP_ENV=production` and production App Attest verification for the shared
server. Do not switch the Health runtime to development or ship activation secrets
inside Journal builds.

## Grant a known development device

`cmd/journaldev` is an operator command using `DATABASE_DSN` from the server runtime.
Build it for the server architecture and run it with the existing private database
network and environment. Do not print or copy the environment into source control.

- With no arguments it lists SHA-256 fingerprints of Journal keys that have
  actually authenticated. It does not grant access.
- `journaldev -grant -device <exact-lowercase-sha256> -days 30` grants access to
  that one existing Journal identity. The duration must be 1–30 days.
- The operation refuses unknown or unused keys and refuses to overwrite a
  StoreKit entitlement. It cannot grant Health access.
- The record uses `environment=development`, includes a voice allowance anchor,
  and retains that anchor on renewal. Existing App Attest, consent, token quotas,
  voice-minute quotas and expiry checks continue to apply.
- Reinstalling with a new App Attest identity requires a new explicit grant.
  Use the device's diagnostic fingerprint or a known test session to identify it;
  do not grant every key found in the database.

In the DEBUG app, enable local subscription access in Settings → Developer Options,
then use “检查云端权益”. This sends an authenticated quota request and does not
upload a journal. A missing activation secret no longer sends the user to a route
that is intentionally absent in production.

## 2026-09-07 investigation

Public Journal HTTPS readiness returned 200. The deployed gateway had voice
processing enabled and its speech and Ark credentials configured. Of seven
Journal key records, only one had completed assertions; it had no managed
entitlement. It was last authenticated on 2026-09-06. The operator granted that
existing test identity 30 days of development access and read the record back.
Its grant expires on 2026-10-07 at 21:00 China time. Unused keys stayed unentitled.
No production HTTP authorization policy or running application image was changed.

From an isolated temporary container with the running gateway's environment:

- `TestLiveSpeechAndDiaryRewrite`: passed; synthetic 16-kHz PCM received 22 speech
  frames, 36 transcript characters and one valid rewritten paragraph; model
  tokens 669 input / 85 output. The park, tea and corrected name were retained.
- `TestLiveJournalOrganization`: passed for both configured models; Lite tokens
  565 / 187 and Pro tokens 565 / 204.

These checks use fixed synthetic content only. They verify provider connectivity
and contracts, not a microphone or an Apple-attested simulator request.
`TestDevelopmentVoiceGrantUsesDeviceOwnerAndExpires` separately exercises HTTP
voice admission for a development grant and verifies denial after expiry.
