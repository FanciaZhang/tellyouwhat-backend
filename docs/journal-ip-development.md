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

The deployed gateway image is `caf942fe5aee2ec83050fea7d603c0d3428e8627` at the time
this IP route was added. It predates the local voice feature commits. Speech
credentials and a voice model are not configured. Therefore the successful IP
route does **not** demonstrate a live ASR/manuscript session. Deploy the voice
milestone and configure its dependencies before that acceptance. Do not bypass
App Attest or create fictitious paid entitlements to make the test pass.

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
- The iPhone reached `/readyz` over trusted HTTPS. Its first authentication attempt
  exposed client decoding of Go's fractional-second challenge expiry; the client
  fix is being verified. Full App Attest reacceptance is waiting for an unlocked
  device, so authentication is not yet reported as passed.
