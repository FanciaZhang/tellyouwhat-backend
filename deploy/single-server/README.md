# 火山引擎单机生产部署

推荐入口是 `https://api.health.tellyouwhat.cn`。API 版本继续放在路径 `/v1/...`；`health.tellyouwhat.cn` 预留给产品介绍、隐私政策、使用条款和支持页面。域名注册商、DNS 权威服务商和服务器厂商彼此独立，不需要为了使用火山引擎迁移域名。

## 1. 购买与网络

首发使用火山引擎中国大陆 ECS：2 vCPU、4 GiB 内存、80 GiB SSD 云盘、固定 EIP，Ubuntu 24.04 LTS，包年包月至少三个月。安全组只开放：

- TCP 22：仅自己的固定公网 IP；
- TCP 80、443：互联网；
- UDP 443：互联网，用于 HTTP/3，可不开放；
- 不开放 3306、6379、8080、8081。

中国大陆服务器在开通域名访问前需要完成备案。已有备案从其他接入商切到火山引擎时，在火山引擎办理接入备案，不需要注销原备案。备案期间不要提前开放站点。

## 2. DNS 与 TLS

先确认权威 DNS：

```sh
dig +short NS tellyouwhat.cn
```

在返回的权威 DNS 服务商控制台添加记录，不一定是在域名注册商控制台添加：

| 主机记录 | 类型 | 值 | 切换期 TTL |
| --- | --- | --- | --- |
| `api.health` | A | 火山 ECS 固定 EIP | 300 秒 |
| `admin.health` | A | 火山 ECS 固定 EIP | 300 秒 |
| `health` | A 或 CNAME | 后续官网地址 | 300 秒 |

不要把 API 放在 `tellyouwhat.cn/health`。子域名可以独立迁移、限流、换证书和排障，也不会耦合现有主站。Caddy 会在 80/443 可访问、DNS 已生效且备案符合要求后自动申请和续期证书。

## 3. 安装与初始化

安装 Docker Engine 和 Compose 插件，将仓库放在服务器固定目录，然后：

```sh
cd Backend
cp deploy/single-server/production.env.example .env.production
mkdir -p secrets
chmod 600 .env.production
```

填写 `.env.production`，并把以下文件放入 `Backend/secrets/`：

- `SubscriptionKey.p8`：App Store Connect 的 In-App Purchase 私钥；
- `MarketingKey.p8`：只供管理后台使用的 App Store Connect Marketing 私钥；
- `apple-app-store-roots.pem`：Apple 官方 PKI 根证书 PEM 合集。

密钥使用火山 IAM 子用户的最小 TOS 权限，不使用主账号长期 AK/SK。TOS bucket 必须私有，并给 `ai-temp/` 配置最长 24 小时生命周期。

检查配置并启动数据库：

```sh
docker compose --env-file .env.production -f compose.production.yaml config
docker compose --env-file .env.production -f compose.production.yaml up -d mysql redis
docker compose --env-file .env.production -f compose.production.yaml run --rm migrate
docker compose --env-file .env.production -f compose.production.yaml up -d --build gateway worker admin caddy
```

验收：

```sh
curl -fsS https://api.health.tellyouwhat.cn/healthz
curl -fsS https://api.health.tellyouwhat.cn/readyz
curl -fsS https://admin.health.tellyouwhat.cn/healthz
docker compose --env-file .env.production -f compose.production.yaml ps
```

App Store Connect 的 Production 和 Sandbox Server Notifications V2 URL 都填写：

```text
https://api.health.tellyouwhat.cn/v1/app-store/notifications
```

管理后台首次上线时先保持 `ADMIN_WRITES_ENABLED=false`，确认 Offer 只读同步正常后，再创建唯一管理员：

```sh
docker compose --env-file .env.production -f compose.production.yaml run --rm adminctl bootstrap
```

详细操作、恢复码保管和 Offer 发布验收见 `Docs/app-store/ManagedAIOfferCodeRunbook.md`。

## 4. 日常维护

每天运行数据清理：

```sh
cd Backend
docker compose --env-file .env.production -f compose.production.yaml run --rm maintenance
```

每天备份 MySQL：

```sh
sudo HEALTH_BACKUP_DIR=/var/backups/health-ai deploy/single-server/backup-mysql.sh
```

本地脚本保留 14 天；初期可按需将备份复制到已有腾讯 COS。Redis 使用 AOF 和持久卷，但它只保存短期 nonce 与额度计数，核心持久数据在 MySQL 和 TOS。

严禁运行 `docker compose down -v`，该命令会删除数据库和 Redis 持久卷。升级顺序：构建镜像、运行迁移、滚动重建 worker/gateway/caddy、检查 `/readyz`。

## 5. 扩容路径

MySQL 和 Redis 当前都以容器运行，不产生单独的托管服务费用。MySQL 保存 App Attest 身份、订阅权益、加密后台任务、可靠队列和用量账本；Redis 提供一次性防重放和原子限流。

容量上升时按此顺序处理：

1. 初期增大 ECS 和云盘；
2. 在个人实名认证账号中购买同地域火山云数据库 MySQL 8.4；
3. 小规模可用 `mysqldump` 导出并导入托管实例；如当时 DTS 支持源端与目标端版本，再用 DTS 做全量迁移和增量同步；
4. 短暂停写并完成最终数据校验后，将 `DATABASE_DSN` 切换到托管实例内网地址；
5. 稳定运行后删除 ECS 中的 MySQL 容器，Redis 可继续同机或再迁到托管 Redis。

自建库从首日启用 `ROW` Binlog 和完整行镜像，表均使用 InnoDB，并具备主键或唯一非空索引，保留后续逻辑导入或 DTS 增量迁移的两种路径。
