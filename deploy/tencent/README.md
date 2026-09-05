# Tencent Cloud production deployment

## 1. Topology

首发只需一台腾讯云 CVM 或轻量服务器运行 Caddy、gateway、worker 与 admin。MySQL 和 Redis 使用已连通的同地域托管实例，应用不在服务器自建持久数据库。所有 MySQL 行和 Redis key 均按 `app_id` 隔离，两个 App 共用实例不会共用权益或业务数据。

应用服务器、MySQL 与 Redis 应放在同一地域并走私网；广州地域完全可以使用，只要三个资源实际互通。TOS 与方舟继续使用火山引擎北京地域：客户端直传临时媒体到 TOS，方舟通过短期签名 URL 读取对象，应用服务器只签发授权和保存元数据。

安全组只开放：

- TCP 22：仅管理员固定公网 IP；
- TCP 80、443：互联网；
- UDP 443：可选，用于 HTTP/3；
- 不对公网开放 3306、6379、8080、8081、8082。

MySQL 应创建专用应用账号，只授予目标数据库读写与迁移所需权限，不使用 `root`。Redis 必须设置密码。两者均只允许应用服务器私网地址或安全组访问。

## 2. DNS, TLS and filing

域名和 DNS 使用现有服务商，不需要迁移。备案/接入备案通过后，在当前权威 DNS 服务商把下列 A 记录指向应用服务器固定公网 IP：

| 主机记录 | 完整域名 | 用途 |
| --- | --- | --- |
| `api.health` | `api.health.tellyouwhat.cn` | 告你健康 API |
| `api.journal` | `api.journal.tellyouwhat.cn` | 告你手记 API |
| `admin` | `admin.tellyouwhat.cn` | 共用管理后台 |

切换期 TTL 可设为 300 秒。Caddy 在 DNS 生效且 80/443 可访问后自动申请和续期 TLS 证书。App Store Server Notifications V2 分别配置：

```text
https://api.health.tellyouwhat.cn/v1/app-store/notifications
https://api.journal.tellyouwhat.cn/v1/app-store/notifications
```

## 3. GitHub production environment

`.github/workflows/backend.yml` 是唯一生产发布入口。GitHub `production` Environment 需要：

Repository variable:

- `PRODUCTION_DEPLOY_ENABLED`：设为 `true` 后，`main` push 自动发布，定时运维检查同时启用。
- `PRODUCTION_ACCEPTANCE_STAGE`：`internal` 自动发布并验收服务器内部服务；`public` 额外验收三个公网 HTTPS 入口。备案完成前使用 `internal`，备案和首次公网验收完成后切换为 `public`。这两个变量必须设置在仓库级。

Environment variables:

- `PRODUCTION_HOST`
- `PRODUCTION_USER`

Secrets:

- `PRODUCTION_SSH_PRIVATE_KEY`：Actions 专用部署私钥
- `PRODUCTION_SSH_KNOWN_HOSTS`：固定服务器 host key
- `PRODUCTION_ENV_FILE`：按 [`../single-server/production.env.example`](../single-server/production.env.example) 填写的完整生产环境文件
- `HEALTH_APP_STORE_PRIVATE_KEY`
- `JOURNAL_APP_STORE_PRIVATE_KEY`
- `HEALTH_APP_STORE_CONNECT_PRIVATE_KEY`
- `JOURNAL_APP_STORE_CONNECT_PRIVATE_KEY`

四个 Apple Secret 必须保存完整 PEM/P8 内容。工作流在临时 Runner 生成权限为 600 的文件，经 SSH 上传到 `/opt/tellyouwhat/backend`。服务器的密钥目录权限为 750、密钥文件权限为 640，所属组为容器的非 root GID `65532`；部署用户需要执行 `sudo -n chgrp 65532` 的权限。环境文件保持 600。任何真实密钥都不能提交到 Git、写入镜像或输出到日志。

## 4. CI/CD flow

Pull Request 与 `main` push 会执行：

1. 使用独立 MySQL 8.4、Redis 8 的 Go 测试、`go vet`、所有命令构建、OpenAPI 生成一致性检查和 Swift contract 测试；
2. gateway、worker、admin、adminctl、migrate、maintenance 六个容器镜像构建；
3. 非 PR 构建发布提交 SHA 和 `main` 标签到 GHCR；
4. 仅当 production gate 开启或手动选择部署阶段时，将 Compose、Caddyfile、部署脚本、生产环境与四个 Apple 私钥上传到独立候选目录；
5. 创建加密数据库备份，拉取绑定提交 SHA 的镜像集并运行迁移，保存旧配置后再切换 gateway、worker、admin，验证两个 App、Worker 和管理后台；
6. 完整公网发布再启动 Caddy，由独立的 `verify-public` job 验证三个域名的真实 DNS、可信 TLS、HTTP 200 和 `status: ready` 响应。

手动运行仅允许 `main` 分支，`deployment` 输入有三个值：

| 值 | 行为 | 验收结果 |
| --- | --- | --- |
| `none` | 测试、构建和发布镜像 | 镜像可交付 |
| `internal` | 额外部署并验证内部服务 | 内部服务就绪，公网验收待完成 |
| `public` | 部署内部服务、启动公网代理并验证三个 HTTPS 入口 | 公网入口验收通过 |

```sh
gh workflow run backend.yml --repo FanciaZhang/tellyouwhat-backend --ref main -f deployment=internal
```

内部验收不依赖 DNS 或证书签发，不启动 Caddy，也不占用公网 80/443。已运行的公网代理不会因选择内部验收而关闭；该模式仍然更新同一套应用服务。

内部验收通过后，部署脚本把提交 SHA 和镜像仓库前缀保存到服务器 `.env.production`。后续 `adminctl` 和 `maintenance` 使用同一版本镜像。迁移失败不会覆盖活动配置；切换后的就绪检查失败会恢复先前的镜像、环境、Compose 和密钥文件，并再次检查恢复后的服务。

首次部署前，服务器安装 Docker Engine、Compose 插件、Python 3.10 以上、curl、jq、OpenSSL，并创建固定目录。腾讯云大陆实例可使用内网 Docker Hub 镜像加速和腾讯 Go module proxy：

```sh
sudo install -d -m 755 /opt/tellyouwhat/backend/deploy/single-server
sudo install -d -m 700 /opt/tellyouwhat/backend/secrets
sudo chown -R "$USER":"$USER" /opt/tellyouwhat/backend
```

生产部署不需要在服务器保留 Git checkout，也不在服务器现场编译。

## 5. Acceptance

MySQL 必须使用符合共享后端 schema 的独立数据库。迁移会先检查核心业务表的 `app_id` 隔离列；即使迁移编号已经登记，也会拒绝旧单 App schema，且不修改其表结构或迁移记录。旧单 App 数据库不能直接作为共享后端数据库；不要通过删除旧表或迁移记录来使检查通过。

先验证私网依赖：

```sh
nc -vz 10.0.0.6 3306
nc -vz 10.0.0.4 6379
```

内部部署后验证：

```sh
curl -fsS -H 'Host: api.health.tellyouwhat.cn' http://127.0.0.1:18080/readyz
curl -fsS -H 'Host: api.journal.tellyouwhat.cn' http://127.0.0.1:18080/readyz
curl -fsS http://127.0.0.1:18081/healthz
curl -fsS -H 'Host: admin.tellyouwhat.cn' http://127.0.0.1:18082/readyz
```

`PUBLIC_PROXY_MODE=docker` 由 Compose 中的 Caddy 管理公网 80/443。服务器已有原生 Caddy 或其他代理时，设置 `PUBLIC_PROXY_MODE=external`，发布流程保留现有代理，只更新和验证内部服务；独立的 `verify-public` job 仍须通过三个公网 HTTPS 入口的验收。

原生 Caddy 可在现有配置中导入 [`Caddyfile.external`](../single-server/Caddyfile.external)，保留原有站点。该文件将两个 App 的域名转发到 `127.0.0.1:18080`，管理后台转发到 `127.0.0.1:18082`，并屏蔽 App 入口的 `/internal/*`。将 `HEALTH_API_DOMAIN`、`JOURNAL_API_DOMAIN`、`ADMIN_DOMAIN` 和 `ACME_EMAIL` 渲染为实际值，或仅将这四个非密钥变量提供给 Caddy 服务。不要让 Caddy 加载完整生产环境文件。先用 `caddy validate` 验证包含原站点的完整配置，再通过 `systemctl reload caddy` 加载；核对原站点仍正常响应。

DNS、备案接入和证书签发条件满足后，执行完整公网发布：

```sh
gh workflow run backend.yml --repo FanciaZhang/tellyouwhat-backend --ref main -f deployment=public
```

公网验证也可独立执行：

```sh
bash deploy/tencent/verify-public.sh api.health.tellyouwhat.cn api.journal.tellyouwhat.cn admin.tellyouwhat.cn
docker compose --env-file /opt/tellyouwhat/backend/.env.production \
  -f /opt/tellyouwhat/backend/compose.production.yaml ps
```

随后执行真实 App Attest、StoreKit Sandbox、免费记餐、订阅 AI、Journal 整理、TOS 上传与数据删除旅程。Development App Attest 使用独立开发配置，生产服务只接受 Production App Attest。镜像发布成功或 `/readyz` 成功不能替代这些业务验收。

## 6. Operations

发布成功后安装三个 systemd timer，均使用北京时间：每日 03:05 加密备份、每日 03:25 保留期清理、周日 04:05 隔离恢复演练，允许两分钟随机延迟。补跑由 `Persistent=true` 管理。运行记录保存在权限受限的 `.operations`；备份位于 `/var/backups/tellyouwhat`，保留 14 天。

定时任务以后端部署目录的非 root 所有者运行。systemd 在任务启动时通过 `LoadCredential` 提供只读配置副本，并将 `TELLYOUWHAT_ENV_FILE` 指向该副本；Python 运行参数和 Compose 的容器 `env_file` 使用同一份配置。仅运行此定时服务时，原 `.env.production` 可为 `root:root 0600`。需要支持服务凭据的 systemd；当前服务器为版本 255。机制见 [systemd 服务凭据](https://systemd.io/CREDENTIALS/)。

直接 SSH 运维和发布由 `PRODUCTION_USER` 执行，需要该用户读取生产配置，并能够写入镜像版本、运行文件及 `.operations`。此模式下 `.env.production` 应归部署用户所有，保持 `0600`；部署记录、`.releases` 及当前和上一个回滚快照也须由同一用户访问。环境文件和密钥不应放宽为其他用户可读。修复历史 root 部署的归属偏差时，仅调整已核实的运行配置、记录和对应快照，保留内容、现有文件权限及容器密钥组，不对整个部署目录递归修改。

更新 `compose.production.yaml`、`ops_common.py` 与 `install-operations.sh` 后，重新执行 `deploy/tencent/install-operations.sh` 安装定时服务。保留服务器已有时区挂载和其他运行配置，按修复范围替换。核验应包含非 root 服务的配置读取、`docker compose config --quiet`、真实加密备份与隔离恢复演练；仅有 timer 列表不能证明任务执行成功。

清理任务先删除过期 TOS 对象，再删除数据库元数据；存储故障会保留记录以便重试。身份清理必须同时满足 30 天未活动、无有效权益、无待删除媒体，且仅操作所属 App 的身份。

`Backend Operations` 每 15 分钟从 GitHub Runner 通过固定 SSH host key 检查服务及依赖、磁盘余量和备份/清理/恢复记录的新鲜度。失败产生失败的 Actions 运行和错误注释；收件人应在 GitHub 通知设置中启用 Actions 失败通知。公开仓库长时间无活动时 GitHub 可能暂停计划运行，服务器本地的三个 timer 不受影响。云平台主机告警可作为独立通道。

手动检查和演练：

```sh
gh workflow run operations.yml --repo FanciaZhang/tellyouwhat-backend --ref main -f operation=health
gh workflow run operations.yml --repo FanciaZhang/tellyouwhat-backend --ref main -f operation=backup
gh workflow run operations.yml --repo FanciaZhang/tellyouwhat-backend --ref main -f operation=restore
gh workflow run operations.yml --repo FanciaZhang/tellyouwhat-backend --ref main -f operation=providers
```

`restore` 校验备份的 HMAC、文件摘要、表集合和每张表的行数，导入临时 MySQL 容器的 tmpfs；该容器无网络和生产数据库挂载，结束后删除。`providers` 使用少量合成文本和图片验证 TOS 私有读写及 Health/Journal 方舟模型，不执行用户购买或修改真实权益。

镜像及配置回滚：

```sh
gh workflow run operations.yml --repo FanciaZhang/tellyouwhat-backend --ref main -f operation=rollback
```

回滚仅恢复上一版已验证的代码与运行配置，不回退数据库迁移。迁移必须保持前后版本兼容；涉及破坏性 schema 变更时应另行准备数据迁移与恢复方案。前一版恢复失败时，会重新启动当前版并检查就绪。再次执行回滚可以切回刚才的版本。每次成功的发布记录均包含两版的准确 SHA。

托管 MySQL 与 Redis 不属于 Compose 项目，更新或清理应用容器不会删除数据库。服务器重启后 Docker 自动启动三个应用容器，systemd 恢复定时任务。依赖故障应由就绪检查报错并保留失败任务的重试状态；不要用删除数据库或清空队列处理依赖故障。

首次管理员创建与应急恢复命令见 [`../single-server/README.md`](../single-server/README.md)。
