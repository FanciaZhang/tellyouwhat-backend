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

- `PRODUCTION_DEPLOY_ENABLED`：首次公网验收前保持 `false`；设为 `true` 后，`main` push 自动执行完整公网发布。该变量用于 job 启动条件，必须设置在仓库级。

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
4. 仅当 production gate 开启或手动选择部署阶段时，上传 Compose、Caddyfile、部署脚本、生产环境与四个 Apple 私钥；
5. 服务器拉取绑定提交 SHA 的镜像集并运行迁移，启动 gateway、worker、admin，验证两个 App 的就绪状态、Worker 存活和管理后台就绪状态；
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

内部验收通过后，部署脚本把提交 SHA 和镜像仓库前缀保存到服务器 `.env.production`。后续 `adminctl` 和 `maintenance` 使用同一版本镜像；内部验收失败不会更新保存的版本。

首次部署前，服务器安装 Docker Engine、Compose 插件、curl、jq、OpenSSL，并创建固定目录。腾讯云大陆实例可使用内网 Docker Hub 镜像加速和腾讯 Go module proxy：

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

公网 80/443 交由本部署的 Caddy 管理。若服务器已有原生 Caddy 或其他代理，先完成现有站点的配置迁移与端口归属核对，再选择 `public`；不要直接停掉服务于其他站点的代理。

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

每天运行 maintenance。托管 MySQL 与 Redis 不属于 Compose 项目，更新或清理应用容器不会删除数据库。首发先使用云数据库自动备份；扩大规模时只需水平扩展无状态 gateway/worker，数据库地址和 App 合约无需改变。

首次管理员创建与应急恢复命令见 [`../single-server/README.md`](../single-server/README.md)。
