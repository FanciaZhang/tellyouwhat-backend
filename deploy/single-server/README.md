# One-server application topology

一台 CVM 或轻量服务器只运行无状态应用容器；MySQL 和 Redis 使用同地域私网中的托管实例：

```text
Internet
   |
 Caddy :443
   |-- api.health.tellyouwhat.cn  ---\
   |-- api.journal.tellyouwhat.cn ---- gateway :8080 ---- MySQL / Redis / TOS / Ark
   `-- admin.tellyouwhat.cn       ---- admin :8082
                                           worker :8081 (private)
```

Caddy 保留原始 Host，gateway 因而能在认证和存储访问之前选定 App。Health 与 Journal 使用两个显式站点块，通过 Caddy snippet 复用安全 Header 和反向代理配置；以后可以按 App 单独增加限流、超时或上传约束。`/internal/*` 不对公网开放；gateway、worker 与 admin 只在 Docker network 内通信，只有 Caddy 暴露 80/443。

服务器固定目录为 `/opt/tellyouwhat/backend`。该目录包含 `compose.production.yaml`、Caddyfile、未纳入版本控制的 `.env.production` 和 `secrets/`：

```sh
cd /opt/tellyouwhat/backend
cp deploy/single-server/production.env.example .env.production
mkdir -p secrets
chmod 600 .env.production secrets/*.p8
docker compose --env-file .env.production -f compose.production.yaml config
```

四个私钥文件分别是：

- `health-subscription.p8`
- `journal-subscription.p8`
- `health-marketing.p8`
- `journal-marketing.p8`

Apple 公共根证书、App Attest 根证书和 Health schema manifest 已固定打入服务镜像，不作为 GitHub Secret 上传。

首次发布先执行迁移，再无构建启动服务：

```sh
docker compose --env-file .env.production -f compose.production.yaml run --rm --no-deps migrate
docker compose --env-file .env.production -f compose.production.yaml up -d --no-build gateway worker admin caddy
```

创建首位 Passkey 管理员：

```sh
docker compose --env-file .env.production -f compose.production.yaml run --rm --no-deps adminctl bootstrap
```

命令会输出一个 15 分钟有效、仅可使用一次的设置链接。最后一位管理员丢失全部 Passkey 时，只能在服务器执行：

```sh
docker compose --env-file .env.production -f compose.production.yaml run --rm --no-deps adminctl users
docker compose --env-file .env.production -f compose.production.yaml run --rm --no-deps adminctl recover <user-id>
```

完整权限与恢复边界见 [`../../docs/modules/admin.md`](../../docs/modules/admin.md)。

分别验收三个公网运行时：

```sh
curl -fsS https://api.health.tellyouwhat.cn/readyz
curl -fsS https://api.journal.tellyouwhat.cn/readyz
curl -fsS https://admin.tellyouwhat.cn/readyz
```

每天运行一次保留期清理：

```sh
docker compose --env-file .env.production -f compose.production.yaml run --rm maintenance
```

托管 MySQL 的备份应优先使用云数据库自动备份。确需补充服务器侧逻辑备份时，设置 `TELLYOUWHAT_BACKUP_DIR` 后运行 `deploy/single-server/backup-mysql.sh`。`.env.production`、`.p8`、备份文件和 registry credential 均不得进入源码仓库。
