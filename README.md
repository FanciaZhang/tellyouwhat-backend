# Tellyouwhat Backend

这是“告你健康”、“告你手记”以及后续 App 共用的独立后端仓库。首发采用模块化单体：一套 gateway、worker、admin 和 migration 代码复用同一组托管 MySQL、Redis、TOS 与方舟基础设施，同时按 App 隔离身份、权益、数据、密钥、模型和配额。

## App 边界

gateway 在认证和读取请求体之前，仅根据 HTTP `Host` 选择 App：

- `api.health.tellyouwhat.cn` → `health`
- `api.journal.tellyouwhat.cn` → `journal`

请求体、Header 和 Prompt 均不能声明 `app_id`。未知 Host 返回 `421`，不属于当前 App 的操作返回 `422`。MySQL 表和 Redis key 均以服务端选出的 `app_id` 分区；App Attest、App Store、方舟路由与配额也按 App 独立配置。共享基础设施不代表共享用户数据或密钥。

## HTTP 与 AI 合约

公开 HTTP 合约由一份 [`PlatformAPI`](Contracts/HTTP/PlatformAPI/openapi.yaml) 和每个 App 的专属 IDL 组成。PlatformAPI 唯一定义 App Attest、权益、隐私、产品信息、配额和健康检查；[`Health App IDL`](Contracts/HTTP/HealthAPI/app.openapi.yaml) 与 [`Journal App IDL`](Contracts/HTTP/JournalAPI/app.openapi.yaml) 只定义业务操作。构建时生成完整的 [`Health 公共合约`](Contracts/HTTP/HealthAPI/openapi.yaml) 与 [`Journal 公共合约`](Contracts/HTTP/JournalAPI/openapi.yaml)，供 Swift/Web 客户端使用。服务端则把一套生成的 Platform strict Gin Router 与当前 App 的 strict Gin Router 组合到同一个 Host 运行时中，公共接口不在各 App 重复实现。

所有 App Attest 和公共幂等 Header 统一使用 `X-Tellyouwhat-*`。管理后台和 Worker 分别拥有独立的 [`AdminAPI`](Contracts/HTTP/AdminAPI/openapi.yaml) 与 [`WorkerAPI`](Contracts/HTTP/WorkerAPI/openapi.yaml) IDL。仓库不保留手写 `ServeMux`、`gin.WrapH` 或备用路由表。

```sh
make generate-api
make verify-generated
make swift-client
```

Health 只接受代码登记的八个操作：`voice_transcription`、`meal_photo_capture`、`hydration_cup_estimate`、`meal_text_capture`、`meal_decision`、`diet_analysis`、`health_nutrition_analysis`、`health_behavior_analysis`。模型、方舟接入点、密钥、工具与配额均由服务端选择。

Journal 只公开固定的 `journal.organize` AI 操作。它接收标题、正文、标签和手记册上下文，使用服务端版本化 Prompt 与严格 JSON Schema 返回整理建议；客户端不能提交 Prompt、model、provider URL、API key、Header 或 tools。Journal 的所有公开路由与请求/响应也由 IDL 生成，不保留手写兼容路由或备用 JSON 签名。

[`deploy/schema-manifest.json`](deploy/schema-manifest.json) 只管理 Health 的 Prompt 版本、JSON Schema 摘要和模型策略，不是 HTTP IDL，不能与 OpenAPI 合并。

## 运行单元

- `cmd/gateway`：公开 API；先按 Host 选 App，再组合 Platform 与对应 App 的生成路由。
- `cmd/worker`：Health 加密异步 AI 任务；内部请求绑定 App ID 与 Job ID，并由 Worker OpenAPI IDL 生成 strict Gin 路由。
- `cmd/admin`：Passkey 登录、管理员/运营角色、人员与 App Store Connect Offer 管理；全部 API 路由由 Admin OpenAPI IDL 生成。
- `cmd/adminctl`：创建首位管理员和服务器侧应急恢复链接。
- `cmd/migrate`：MySQL 8.4 基线 schema，包含 Health 与 Journal 注册项。
- `cmd/maintenance`：按保留期清理临时媒体、任务、幂等和用量数据。

## 权益、隐私与存储

`GET /v1/products/managed-ai` 返回当前 App 的产品 ID、配额上限、供应商披露和法律链接。价格由 StoreKit 管理，后端不硬编码本地化价格。

生产交易通过 App Attest 保护的 `POST /v1/entitlements/transactions` 同步。后端独立验证 Apple JWS、Bundle ID、环境和当前 App 的产品白名单，并把 original transaction 绑定到已认证设备。Health 支持月订阅和年订阅；免费拍照、文字或语音记餐使用独立的每日三次 recognition session 与安全 token 上限。

敏感 Prompt 和结果在写入 MySQL 前使用 AES-256-GCM 加密。Redis 保存防重放、分布式限流和配额状态。TOS 仅保存 `ai-temp/` 临时媒体，上传授权绑定 MIME、大小和 SHA-256，bucket 必须私有并设置最长 24 小时生命周期。客户端的 BYOK 模式不经过本后端。

## 本地运行

```sh
cp .env.example .env.local
docker compose up -d mysql redis
set -a; source .env.local; set +a
go run ./cmd/migrate
go run ./cmd/gateway
```

本地请求仍需 Development App Attest。直连 localhost 时必须显式携带目标 Host，不存在跳过认证的调试后门。若旧开发数据库运行过早期 schema，应明确重建开发数据库；迁移器不会静默删除旧数据。

## 生产部署

首发使用一台腾讯云 CVM 或轻量服务器运行 Caddy、gateway、worker 和 admin，并连接同地域私网中的托管 MySQL 与 Redis。Caddy 仅开放 80/443；Health 与 Journal 使用显式、独立的站点块并复用公共安全配置，因此以后可以分别配置限流、超时和上传策略，而不需要增加服务器。完整配置、CI/CD、首次管理员和验收步骤见 [`deploy/tencent/README.md`](deploy/tencent/README.md)。

线上版本、候选源码和部署前配置核对见[源码与生产部署对应关系](docs/source-deployment-traceability.md)。

## 验证

```sh
go test ./...
go test -race ./...
go vet ./...
make verify-generated
swift test --package-path Contracts/HTTP
```

测试覆盖 Host/操作隔离、App Attest、防重放、权益、隐私同意、订阅与免费配额、Health/Journal 严格 HTTP 合约、Journal 结构化输出、异步任务、数据分区和管理权限。真实 App Attest、StoreKit Sandbox、TOS、方舟模型、HTTPS 与托管 MySQL/Redis 仍需在生产预备环境和真机完成验收。

CI 为每次运行启动独立的 MySQL 8.4 与 Redis 8 服务，并通过 `MYSQL_TEST_DSN` 和 `REDIS_TEST_URL` 执行真实存储集成测试。本机未提供这两个变量时，对应测试会跳过；MySQL 集成测试会重置测试表，数据库名必须以 `_test` 结尾，禁止使用业务数据库。
