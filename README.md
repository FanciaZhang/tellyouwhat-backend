# Tellyouwhat Backend

这是“告你健康”、“告你手记”以及未来 App 共用的后端仓库。首版是一个模块化单体，在一台服务器上运行 Caddy、gateway、worker 和 admin，并共用一套 MySQL、Redis 和 TOS。添加 App 不需要新增 ECS、MySQL 或 Redis 实例。

## 边界

App 由 HTTP `Host` 唯一决定：

- `api.health.tellyouwhat.cn` → `health`
- `api.journal.tellyouwhat.cn` → `journal`

请求体、Header 和 Prompt 都不允许声明 `app_id`。未知 Host 返回 `421`，操作命名空间与 App 不匹配返回 `422`。

共享的平台能力包括：

- App Attest 注册、断言验证和防重放；
- App Store 交易、权益、Offer 兑换、版本化隐私同意；
- 幂等、限流、token 配额、加密异步任务、临时媒体和用量记账；
- 通行密钥管理后台与按 App 切换的 Offer 管理。

所有持久表和 Redis key 都以 `app_id` 分区。App Attest 的 Team ID、Bundle ID，App Store 的 App Apple ID、订阅产品，火山方舟密钥、模型路由和配额按 App 独立配置。共享基础设施不等于共享数据或密钥。

## AI 接口

管理型 AI 只提供固定业务操作，不提供“任意 Prompt + 任意模型”代理。通用路径是：

```text
POST /v1/ai/operations/{operation}/responses
```

Health 仍由 Swift 根据业务状态组装 Prompt，但只能提交代码登记的 `health.*` 操作、Prompt 版本和 JSON Schema；模型、供应商密钥和工具仍由服务端选择。Health 的 BYOK 模式不经过本后端，用户密钥不上传。

Journal 只提供 `journal.organize`。App 上传标题、正文、现有/已拒绝标签与手记册上下文；服务端用版本化 Prompt 和严格 JSON Schema 请求火山方舟 Responses API，返回最多 8 个标签、3 个已有手记册推荐和 2 个新手记册建议。客户端不能提交 Prompt、model、provider URL、API key、Header 或 tools。

## 运行单元

- `cmd/gateway`：公开 API，先按 Host 选 App，再进入独立运行时。
- `cmd/worker`：Health 的加密异步 AI 任务；内部请求同时绑定 App ID 和 Job ID。
- `cmd/admin`：共用通行密钥登录，按 App 选择 App Store Connect 资源。
- `cmd/adminctl`：生成首个管理员的短期设置链接。
- `cmd/migrate`：干净的 MySQL 8.4 基线 schema，直接包含 Health 和 Journal App 注册项。
- `cmd/maintenance`：全平台的保留期清理。

## 本地运行

```sh
cp .env.example .env.local
docker compose up -d mysql redis
set -a; source .env.local; set +a
go run ./cmd/migrate
go run ./cmd/gateway
```

本地请求仍必须使用真实 Development App Attest。因为 Host 是安全边界，直连 localhost 时需显式传入目标 Host；不存在未鉴权调试后门。

## 单服务器部署

`compose.production.yaml` 在同一 Docker network 运行 Caddy、gateway、worker 和 admin。Caddy 同时代理 Health API、Journal API 和共享管理域名，只对外开放 80/443；应用端口不互相冲突，也不需要第二台 ECS。

部署顺序：先运行 `migrate`，再更新 gateway/worker/admin，最后验证两个 API Host 的 `/readyz`。App Store Server Notifications 必须分别指向两个 App 域名下的 `/v1/app-store/notifications`。

## 验证

```sh
go test ./...
go test -race ./...
go vet ./...
```

本地测试覆盖 Host/操作隔离、App Attest、权益、同意、配额、严格合约、Journal Responses API 结构化输出、幂等任务、数据分区和管理预览的跨 App 绑定。真实 App Attest、StoreKit Sandbox、TOS、方舟模型和 HTTPS 流式输出仍需要云端密钥与真机验收。
