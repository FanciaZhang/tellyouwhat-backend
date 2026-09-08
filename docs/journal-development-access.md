# Private Journal development

Journal's local subscription switch is a DEBUG-only UI simulation. Real voice
and organization tests use `cmd/journaldevserver`, a separate operator-started
process. No Apple sandbox purchase, per-device allowlist, or production SQL
entitlement grant is involved.

The Journal app repository's `scripts/journal-development.py` starts the service
over SSH. `start` creates a fresh 24-hour session; `connect` imports its credential
to a simulator or paired iPhone without rebuilding; `stop` closes it. The process
only listens on `127.0.0.1:18787`. The existing Journal HTTPS site forwards only
`/_development/journal/*` to it. Never open port 18787 directly in a firewall.

## Boundaries

- The production gateway does not import the development package. The production
  Dockerfile does not build its command. Starting it never restarts the shared
  gateway, worker or admin.
- It refuses `APP_ENV=production`, `DATABASE_DSN`, and `REDIS_URL`. Entitlements,
  consent, request deduplication, voice receipts and quotas use private memory.
- A fresh 256-bit random session token authorizes fixed Journal endpoints only.
  It expires within 24 hours; the process also exits at expiry. Apple enrollment,
  purchases, generic Health operations and arbitrary provider proxying are absent.
- WebSockets still require their expiring, single-use voice tickets. Consent is
  enforced by the normal Journal handler. The client refuses credential redirects.
- Per session: 10 minutes of audio, 100,000 organization tokens, two concurrent
  provider operations and a CNY 1 estimated provider budget. Existing conservative
  price bounds and reservation/settlement wrappers cover ASR and both rewrite and
  organization. A fixed accounting epoch prevents a month boundary resetting the
  session's spending ceiling. Provider billing remains authoritative; update the
  upper-bound prices if provider pricing/model selection changes.
- Only the operator can restart the process and reset its in-memory budget. No
  automatic restart policy is set. Use deliberately; it is not an unrestricted staging
  service or a substitute for real StoreKit acceptance.
- The remote launcher forwards only Journal model/ASR configuration into the
  isolated container. No Apple signing, Health, database or Redis credentials are
  copied. Provider credentials never reach the Mac or app. The short-lived
  session credential is stored with owner-only permissions on the Mac and in the
  Debug app's Keychain. Stop or rotate the session to revoke it.

## Retired manual grant

`cmd/journaldev` was removed. Its one manually created development entitlement
was deleted on 2026-09-07 using app, key hash, environment, empty transaction ID
and exact creation-anchor predicates. Verification found zero matching records
remaining; Health entitlement metadata was unchanged. No purchase records or
App Attest registrations were deleted.

## Verification

`go test -race ./cmd/journaldevserver ./internal/journal/development
./internal/journal/provider ./internal/journal/voice ./internal/entitlement
./internal/gateway ./internal/costcontrol`

The boundary tests reject missing/wrong/expired tokens, production configuration,
non-Journal routes, missing consent and using a developer token as a voice ticket.
Live provider checks are explicitly enabled with synthetic input only. Simulator
acceptance is maintained in the app repository.

## 真机 HTTPS 接入

手记的原生 Caddy 站点使用 `handle_path /_development/journal/*`，仅此路径转发
到独立开发容器的 127.0.0.1:18787。正式 gateway、数据库和健康站点不变。
这是带临时认证的公网 HTTPS 入口，不再要求手机通过 Mac 的 SSH 隧道。
启动或连接脚本通过配对设备放入一次性缓存配置；App 打开时导入 Keychain 并删除
配置文件。安装时锁屏不会漏配凭证，不把凭证写进 App 二进制或仓库。
无凭证请求必须返回 401，健康域名下的同一路径必须返回 404。
到期、额度、隐私授权和一次性 WebSocket 票据检查仍由独立开发进程执行。
Caddy 改动需先验证完整配置再热加载；不重启生产业务容器。
