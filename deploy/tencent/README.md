# 腾讯云首发部署

生产入口为 `https://api.health.tellyouwhat.cn`。API 版本继续放在
`/v1/...`；`health.tellyouwhat.cn` 预留给产品介绍、隐私政策、使用条款
和支持页面。

## 1. 资源与网络

- 轻量应用服务器：广州，Ubuntu 24.04 LTS，私网 IP `10.1.0.16`，公网
  IP `106.55.105.177`。
- TDSQL-C MySQL：私网 `10.0.0.6:3306`，数据库 `health_ai`。
- TencentDB for Redis：私网 `10.0.0.4:6379`。
- `health 云连网`：连接轻量服务器 VPC 与 `health-vpc`，路由网段不得
  冲突。

MySQL 和 Redis 不开启公网。它们的安全组只允许来源 `10.1.0.16/32`
分别访问 TCP 3306 和 TCP 6379。轻量服务器只开放 TCP 22、80、443；
3306、6379、8080、8081 均不对公网开放。

## 2. DNS、备案与 TLS

权威 DNS 继续使用 Netlify。在 Netlify DNS 添加：

| 主机记录 | 类型 | 值 | 切换期 TTL |
| --- | --- | --- | --- |
| `api.health` | A | `106.55.105.177` | 300 秒 |
| `admin.health` | A | `106.55.105.177` | 300 秒 |

中国大陆服务器开通域名访问前需完成腾讯云备案或接入备案。DNS 生效、
备案完成且 80/443 可访问后，Caddy 自动申请和续期 TLS 证书。

## 3. 数据库凭据

创建 MySQL 读写账号 `health_ai`，主机限制为 `10.1.0.16`，只授予
`health_ai` 数据库权限。不要给应用使用 `root`。Redis 必须启用密码。
密码只保存在服务器权限为 600 的 `.env.production` 中。

## 4. 安装与配置

安装 Docker Engine 和 Compose 插件。腾讯云中国大陆实例应配置腾讯云
内网 Docker Hub 加速，避免官方仓库的网络超时：

```json
{
  "registry-mirrors": ["https://mirror.ccs.tencentyun.com"]
}
```

将该配置保存为 `/etc/docker/daemon.json` 并重启 Docker。随后将仓库放在
服务器固定目录。腾讯云部署环境中的 `GOPROXY` 使用腾讯云 Go 模块镜像，
避免 `proxy.golang.org` 的网络超时。然后：

```sh
cd Backend
cp deploy/tencent/production.env.example .env.production
mkdir -p secrets
chmod 600 .env.production
```

填写 `.env.production`，并把以下只读文件放入 `Backend/secrets/`：

- `SubscriptionKey.p8`：App Store Connect In-App Purchase 私钥；
- `MarketingKey.p8`：只供 Offer 管理后台使用的 App Store Connect
  Marketing 私钥；
- `apple-app-store-roots.pem`：Apple 官方 PKI 根证书 PEM 合集。

TOS 使用最小权限 IAM 子用户，不使用主账号长期 AK/SK。Bucket 必须为
私有，并为 `ai-temp/` 配置最长 24 小时生命周期。

方舟按业务模块分配 7 个独立推理接入点：语音转写、图片记餐、文本记餐、
下一餐决策、饮食分析、健康营养分析和健康行为分析。首发可全部使用
`Doubao-Seed-2.0-mini`，但不共用接入点；后续只替换单个模块的
`ARK_ENDPOINT_*` 即可切换模型并独立观察用量。生产 API Key 只授权这些
接入点。

## 5. GitHub CI/CD

仓库使用私有 GitHub 仓库，`.github/workflows/backend.yml` 是后端唯一生产
发布入口：

1. PR 和 `main` 分支变更先执行 Go 单元测试、`go vet`、生产命令编译和
   gateway、worker、admin、adminctl、migrate、maintenance 六个镜像构建；
2. `main` 验证通过后，把带提交 SHA 的六个镜像发布到 GHCR；
3. 只有 GitHub `production` Environment 中
   `PRODUCTION_DEPLOY_ENABLED=true`，或手动运行工作流并选择部署时，才会
   连接生产服务器；
4. 部署先拉取完整镜像集并执行数据库迁移，再无构建重启服务，最后通过
   服务器回环地址 `127.0.0.1:18080/readyz` 验收。

GitHub `production` Environment 是生产密钥的唯一配置来源。它保存：

- Secret `PRODUCTION_SSH_PRIVATE_KEY`：专用于 GitHub Actions 的生产部署
  私钥，不复用个人电脑的 SSH 私钥；
- Secret `PRODUCTION_SSH_KNOWN_HOSTS`：固定生产服务器 host key，禁止
  `StrictHostKeyChecking=no`；
- Secrets `MYSQL_PASSWORD`、`REDIS_PASSWORD`；
- Secrets `PAYLOAD_ENCRYPTION_KEY`、`WORKER_INTERNAL_SECRET`、
  `JOB_CAPABILITY_SECRET`；
- Secrets `APP_STORE_ISSUER_ID`、`APP_STORE_KEY_ID`、
  `APP_STORE_APP_APPLE_ID`、`APP_STORE_PRIVATE_KEY`；
- Secrets `APP_STORE_CONNECT_ISSUER_ID`、`APP_STORE_CONNECT_KEY_ID`、
  `APP_STORE_CONNECT_SUBSCRIPTION_ID`、`APP_STORE_CONNECT_PRIVATE_KEY`；
- Secret `ADMIN_PREVIEW_SIGNING_KEY`：至少 32 字节随机值的无填充 Base64；
- Secrets `ARK_API_KEY`、`TOS_ACCESS_KEY`、`TOS_SECRET_KEY`；
- Variable `PRODUCTION_HOST`：`106.55.105.177`；
- Variable `PRODUCTION_USER`：`ubuntu`；
- Variable `PRODUCTION_DEPLOY_ENABLED`：首轮生产验收完成前保持 `false`。

`production.env.template` 只包含可提交的生产配置与密钥占位符。发布时，
`render-production-env.sh` 在 GitHub 托管的临时 Runner 上从 Environment
Secrets 生成权限为 600 的 `.env.production`，并校验 Apple 私钥格式；工作流
通过 SSH 原子替换服务器配置和私钥，随后删除 Runner 临时文件。Apple 公开
根证书随仓库发布，所有私密值均不写入 Git、不打进镜像、不输出到日志。
工作流拉取私有 GHCR 镜像时只临时使用本次任务的 `GITHUB_TOKEN`，完成部署
后立即退出 registry 登录。

## 6. 首次连通性与启动

先从轻量服务器验证私网端口：

```sh
nc -vz 10.0.0.6 3306
nc -vz 10.0.0.4 6379
```

生产发布由 GitHub Actions 手动触发。服务器上的以下命令仅用于故障诊断：

```sh
docker compose --env-file .env.production -f compose.production.yaml config
docker compose --env-file .env.production -f compose.production.yaml run --rm --no-deps migrate
docker compose --env-file .env.production -f compose.production.yaml up -d --no-build gateway worker admin caddy
```

验收：

```sh
curl -fsS https://api.health.tellyouwhat.cn/healthz
curl -fsS https://api.health.tellyouwhat.cn/readyz
curl -fsS https://admin.health.tellyouwhat.cn/healthz
docker compose --env-file .env.production -f compose.production.yaml ps
```

App Store Connect 的 Production 和 Sandbox Server Notifications V2 URL
均填写：

```text
https://api.health.tellyouwhat.cn/v1/app-store/notifications
```

## 7. 日常维护

每天运行数据清理：

```sh
cd Backend
docker compose --env-file .env.production -f compose.production.yaml run --rm maintenance
```

日常升级由 GitHub Actions 完成：验证、发布带提交 SHA 的镜像、运行迁移、
无构建重启 worker/gateway/admin/caddy、检查 API 与管理后台健康
端点。托管 MySQL 和 Redis 不属于 Compose 项目，执行 Compose 清理不会
删除它们的数据。

## 8. 扩容路径

首发阶段先纵向调整轻量服务器、TDSQL-C 和 Redis 规格。免费 CCN 政策
结束前重新比较连接费与 CVM 费用；若改用 `health-vpc` 内的 CVM，只需
迁移无状态的 Caddy、gateway 和 worker，MySQL 与 Redis 地址保持不变。
