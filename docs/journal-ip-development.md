# Journal IP development endpoint

The temporary iPhone development endpoint is `https://106.55.105.177`. It uses
publicly trusted TLS and the existing Journal gateway. `/readyz` is public;
business APIs still require App Attest, consent and the applicable entitlement.
This development route does not complete filing or the formal-domain launch gates.

## Server configuration

- Native Caddy continues to own ports 80/443. The existing static website and API
  domain configurations are retained. Gateway port 18080 remains loopback-only.
- Render `deploy/single-server/Caddyfile.journal-ip`, replacing `__JOURNAL_IP__`
  with the public IPv4 address, and install as `/etc/caddy/journal-ip.caddy`.
- Render `Caddyfile.journal-ip-global` the same way, and install as
  `/etc/caddy/journal-ip-global.caddy`. Import it inside the root Caddyfile's one
  global-options block. IP clients omit SNI, and the server's private listening
  address does not match the public IP certificate; `default_sni` is necessary.

The root configuration includes these additions:

```caddyfile
{
    import /etc/caddy/journal-ip-global.caddy
}

# Existing website and backend imports remain here.
import /etc/caddy/journal-ip.caddy
```

The IP site fixes the upstream Host to `api.journal.tellyouwhat.cn`, forwards real
client IPs and WebSocket upgrades, blocks `/internal` and `/internal/*`, and allows
existing streams 35 minutes to finish after a configuration/certificate reload.
It does not publish the worker, database or administration ports.

## Certificate issuance and renewal

Certbot 5.8.0 is installed in `/opt/tellyouwhat/certbot`, using a Python 3.12 virtual
environment. This is separate from Ubuntu's older distribution package. The ACME
HTTP challenge is served from the existing `/var/www/tellyouwhat/public` webroot.
Staging issuance was tested in `/var/lib/tellyouwhat/ip-cert-staging`; its
untrusted certificate is never loaded by Caddy.

Install `deploy/tencent/journal-ip-certificate-deploy.sh` as root-owned mode 750 at
`/usr/local/sbin/tellyouwhat-journal-ip-certificate-deploy`. Production issuance:

```sh
sudo /opt/tellyouwhat/certbot/bin/certbot certonly \
  --non-interactive --agree-tos --email '<existing ACME email>' \
  --preferred-profile shortlived --webroot \
  --webroot-path /var/www/tellyouwhat/public \
  --ip-address 106.55.105.177 --cert-name journal-ip \
  --deploy-hook /usr/local/sbin/tellyouwhat-journal-ip-certificate-deploy
```

The hook checks the certificate lifetime and public/private key match, copies
certificate files with mode 640 into a root:caddy directory of mode 750, validates
Caddy and reloads it. On failure it restores the prior certificate files. Private
keys stay on the server and are never copied into the repository or App.

Install `journal-ip-certificate.service` and `.timer` from `deploy/tencent` into
`/etc/systemd/system`. Enable the timer with `systemctl enable --now
journal-ip-certificate.timer`. It checks twice daily with a randomized delay;
successful renewal runs the saved deploy hook. Verify with:

```sh
sudo /opt/tellyouwhat/certbot/bin/certbot renew --cert-name journal-ip --dry-run
systemctl list-timers journal-ip-certificate.timer
curl --fail https://106.55.105.177/readyz
```

IP certificates last about six days. Issuance, automatic renewal and Caddy loading
are separate checks. See [Let's Encrypt's IP certificate instructions](https://letsencrypt.org/2026/03/11/shorter-certs-certbot.html)
and [Caddy's default SNI documentation](https://caddyserver.com/docs/caddyfile/options#default_sni).

## App configuration and remaining gates

JournalApp Debug builds bundle the IP HTTPS URL and use production App Attest to
match this gateway. Release builds use the formal domain. The IP ATS entry keeps
HTTPS, forward secrecy and normal system certificate validation. The environment
override remains available for explicitly launched debug sessions; a desktop-icon
launch uses the bundled URL without needing Xcode environment variables.

The full backend now runs `85dd734` (`voice-85dd734`), including the voice feature,
App Attest fix and generated voice API routes. Migration 0002 has been applied.
The existing Journal Pro endpoint supplies the rewrite model. The existing Journal
speech API key has been configured, the synthetic live ASR and rewriting check
passed, and `JOURNAL_VOICE_ENABLED=true` is active on the gateway. Unsigned admission
requests correctly return 401. Paid on-device recording and recovery still need
an active synchronized subscription; provider acceptance does not replace that. Do not bypass
App Attest or create fictitious paid entitlements to make the test pass.

## App Attest assertion hotfix (superseded by the full voice deployment)

The gateway-only override below was retired when `voice-85dd734` was deployed.
Use the standard release rollback described in `journal-voice.md` for the current deployment.


Real iPhone registration succeeded, but authenticated requests returned 401. The
verifier passed Apple's nonce directly to Go's `ecdsa.VerifyASN1`, which expects
the SHA-256 digest of the signed message. Apple signs the nonce using ECDSA-SHA256;
the fix computes `SHA256(SHA256(authenticatorData || clientDataHash))` before
verification. It does not accept the previous digest as a fallback. Request
binding, nonce consumption, RP ID checks and monotonic counters remain enforced.
See [Apple's assertion validation procedure](https://developer.apple.com/documentation/devicecheck/validating-apps-that-connect-to-your-server).

Focused race-enabled tests passed for attestation, contracts and gateway on both
the working branch and the exact deployed base plus this patch. The gateway was
cross-compiled for Linux amd64 from the deployed base plus only this verifier fix,
and copied into the existing gateway image. No other service or schema changed.
The running image is `tellyouwhat-journal-gateway-hotfix:caf942f-88cb4e7`.

`/opt/tellyouwhat/backend/compose.journal-app-attest-hotfix.yaml` overrides only the
gateway image. Binary and source checksums, base/fix commit IDs and the Dockerfile
are recorded at `/var/backups/tellyouwhat-app-attest/20260904-caf942f-88cb4e7/`.
The original gateway image remains available. From `/opt/tellyouwhat/backend`,
reapply the hotfix using:

```sh
docker compose --env-file .env.production -f compose.production.yaml \
  -f compose.journal-app-attest-hotfix.yaml \
  up -d --no-deps --no-build --pull never gateway
```

For emergency rollback, omit the hotfix `-f` argument and run the same command.
This restores the original image, including its known assertion bug. Verify
`/readyz` after either operation. Before the next standard CI deployment, include
commit `88cb4e7` in its source; otherwise that deployment will restore the bug.
Retire the override after the standard image includes the fix. This direct,
gateway-only deployment did not push Git commits or publish registry images.

## Removal after filing

Switch the Debug URL/host back to the formal domain and deliver that App build
first. Remove the two IP imports (including the global block only if otherwise
empty), validate the entire Caddyfile and reload. Disable the dedicated renewal
timer once no development clients use the IP. Keep formal-domain configuration
and the existing blog intact.

The original root Caddyfile was backed up on the server at
`/var/backups/tellyouwhat-ip-endpoint/20260904T135756Z/Caddyfile`. Inspect subsequent
server changes before restoring an entire backup; normally remove only these
imports. Certificate files may be removed separately after the endpoint is retired.

## Verification on 2026-09-04

- Staging and production IP certificate issuance passed. The production leaf has
  an IP SAN and a trusted Let's Encrypt chain; it expires 2026-09-11 04:58:59 UTC.
- Public HTTPS `/readyz`: 200 with `status: ready`, without disabling TLS checks.
- Unauthenticated `/v1/ai/quota` with a valid request ID: 401.
- `/internal/jobs/process`: 404. Existing HTTP IP blog homepage: 200.
- Caddy configuration and systemd units validated; renewal timer enabled.
- Renewal dry run, the deployed certificate hook and the systemd service passed.
- WSS through the same proxy passed a synthetic echo check: trusted TLS 1.3,
  HTTP 101 and matching data in both directions. The temporary route/process were
  removed immediately afterward; its URL now returns 404.
- The iPhone reached `/readyz` over trusted HTTPS. The client fix for Go's
  fractional-second challenge expiry passed regression tests and enabled real
  production App Attest registration (201).
- Subsequent signed quota reads exposed the assertion nonce hashing bug (401).
  The gateway hotfix is deployed and internal/public readiness checks pass.
- Physical-device integration passed: one test, zero failures. Two consecutive
  signed requests passed authentication and returned the exact
  `managed_subscription_required` code (403); this device has no synchronized
  active entitlement. The same unsigned endpoint returned 401. This validates
  authentication and the subscription boundary, not a paid quota response.
- The App's normal synchronization of its existing consent also returned 200
  after the verifier fix. The integration test itself only performs reads and
  App Attest operations; it does not grant consent or alter purchases.
