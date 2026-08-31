# One-server topology

The production topology needs one ECS/Lighthouse instance only:

```text
Internet
   |
 Caddy :443
   |-- api.health...  ----\
   |-- api.journal... ----- gateway :8080 ---- MySQL / Redis / TOS / Ark
   `-- admin...       ----- admin :8082
                                  worker :8081 (private)
```

Caddy preserves the original Host so the gateway can select the App before authentication and storage access. `/internal/*` is blocked publicly. Gateway, worker, and admin share one Docker network; only Caddy exposes public ports.

Install `compose.production.yaml`, this Caddyfile, an untracked `.env.production`, and an untracked `secrets/` directory under `/opt/tellyouwhat/backend`. Run:

```sh
docker compose --env-file .env.production -f compose.production.yaml run --rm --no-deps migrate
docker compose --env-file .env.production -f compose.production.yaml up -d gateway worker admin caddy
```

After the first clean migration, create the initial passwordless administrator:

```sh
docker compose --env-file .env.production -f compose.production.yaml run --rm --no-deps adminctl bootstrap
```

The command prints a 15-minute, single-use Passkey setup URL. If the final
administrator later loses every Passkey, list the management users and issue a
short-lived recovery enrollment URL from the server:

```sh
docker compose --env-file .env.production -f compose.production.yaml run --rm --no-deps adminctl users
docker compose --env-file .env.production -f compose.production.yaml run --rm --no-deps adminctl recover <user-id>
```

There is no password, TOTP, SMS, or public recovery endpoint. See
[`../../docs/modules/admin.md`](../../docs/modules/admin.md) for the complete
role, invitation, session, and recovery boundaries.

Verify all public runtimes independently. The Health readiness check also
verifies that the private asynchronous worker is reachable:

```sh
curl -fsS https://api.health.tellyouwhat.cn/readyz
curl -fsS https://api.journal.tellyouwhat.cn/readyz
curl -fsS https://admin.tellyouwhat.cn/readyz
```

Use `TELLYOUWHAT_BACKUP_DIR=/var/backups/tellyouwhat deploy/single-server/backup-mysql.sh` for database backups. Keep `.env.production`, Apple `.p8` files, backup archives, and registry credentials outside source control.
