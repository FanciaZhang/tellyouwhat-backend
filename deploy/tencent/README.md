# Tencent single-instance deployment

One ECS/Lighthouse instance runs Caddy, gateway, worker, and admin. The existing private MySQL and Redis instances are shared by every App; all application records and Redis keys are scoped by `app_id`.

GitHub Actions publishes six service images and, when the production gate is enabled, uploads `compose.production.yaml`, the Caddyfile, one protected environment file, and App-specific Apple keys to `/opt/tellyouwhat/backend`. The deployment runs the clean migration before restarting services.

Required GitHub Environment values:

- variables: `PRODUCTION_HOST`, `PRODUCTION_USER`, `PRODUCTION_DEPLOY_ENABLED`;
- secrets: `PRODUCTION_SSH_PRIVATE_KEY`, `PRODUCTION_SSH_KNOWN_HOSTS`, `PRODUCTION_ENV_FILE`;
- App Store subscription keys: `HEALTH_APP_STORE_PRIVATE_KEY`, `JOURNAL_APP_STORE_PRIVATE_KEY`;
- App Store Connect admin keys: `HEALTH_APP_STORE_CONNECT_PRIVATE_KEY`, `JOURNAL_APP_STORE_CONNECT_PRIVATE_KEY`.

`PRODUCTION_ENV_FILE` follows the checked-in `.env.example`, with production values and secret paths under `/secrets`. It must define both API domains and the shared admin domain. Never commit it.

After the first clean deployment, use the `adminctl` Compose service to create
the initial Passkey administrator. The same server-only command is the final
administrator recovery path; there is no public password or recovery-code
endpoint. Commands and security boundaries are documented in
[`../single-server/README.md`](../single-server/README.md) and
[`../../docs/modules/admin.md`](../../docs/modules/admin.md).
