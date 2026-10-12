# 生日心意助手接入

生日助手是独立 App 身份，复用平台的 App Attest 注册、按 App 隔离的存储和额度、项目成本控制。接口为 `POST /v2/birthday/assist`，操作只允许 `birthday.propose` 和 `birthday.revise`。首块采用受限免费额度，不要求 StoreKit 订阅，也不触碰手记和健康权益。

默认 `BIRTHDAY_AI_ENABLED=false`，已有服务的部署配置继续只启用原来的 App。生日需显式配置自己的 Bundle ID、Apple Team、Ark 凭据／模型和成本预留价格；配置不会隐式取健康或手记的值。

## 2026-10-12 开发接入

开发服务使用 `https://api.journal.tellyouwhat.cn/_development/birthday/`，由现有可信 HTTPS 站点转发到独立生日进程的回环端口 `18788`。代理删除入口前缀，内部 Host 固定为 `birthday.development`；签名仍使用 `/v2/birthday/assist` 等业务路径。只开放就绪、App Attest 注册与生日决策接口。

运行配置设置 `BIRTHDAY_AI_ONLY=true`，仅加载生日。开发进程不持有健康、手记、TOS 或控制面密钥；服务端明确复用既有 Ark 推理凭据和 Pro 推理端点，客户端只持有生日开发注册凭据。身份、额度和用量以 `birthday` 隔离，成本受现有共享项目预算约束。当前用每百万输入／输出 token 9.6／48 元作保守成本预留，这不是供应商账单。

服务按 `compose.birthday-development.yaml` 运行，只绑定回环端口。后端代码位于 `codex/birthday-real-ai` 分支；该开发部署使用固定源码提交编译的 Linux amd64 二进制，不替换其他 App 的运行镜像。HTTP 合同位于 `Contracts/HTTP/BirthdayAPI/app.openapi.yaml`，使用现有 `openapi-compose` 和 Gin 生成器，纳入全量 Go 测试与生成检查。

客户端 Debug 使用 `Configuration/AI.Debug.xcconfig`，开发注册凭据位于被 Git 忽略的 `Configuration/AI.local.xcconfig`，文件权限为 `0600`。Release 不读取该配置，当前正式服务地址仍为空。正式接入需完成生日域名 DNS/TLS、Production App Attest 与正式设备验收。

设备需使用 `cn.tellyouwhat.birthday`、团队 `T6DD96HY2B`、Development App Attest，允许的开发构建号为 `1`。模拟器不支持 App Attest。开发注册凭据留在受限配置文件，供应商密钥不进入 App 或仓库。

## 配置

| 变量 | 用途 |
|---|---|
| BIRTHDAY_AI_ENABLED | 显式启用，默认 false |
| BIRTHDAY_AI_ONLY | 独立部署只加载生日，必须同时启用 AI；无需其他 App 或 TOS 凭据 |
| BIRTHDAY_API_HOSTS | 生日专用 HTTPS 主机，与健康和手记分开 |
| BIRTHDAY_APPLE_TEAM_ID | 生日 App 的签名团队 |
| BIRTHDAY_APPLE_BUNDLE_ID | cn.tellyouwhat.birthday |
| BIRTHDAY_APP_ATTEST_ENV | 与签名环境一致；生产必须 production |
| BIRTHDAY_ALLOWED_APP_BUILDS | 开发设备允许注册的构建号 |
| BIRTHDAY_DEV_ACTIVATION_SECRET | 仅开发注册使用的专用凭据 |
| BIRTHDAY_ARK_BASE_URL | Ark Responses API 根地址 |
| BIRTHDAY_ARK_API_KEY | 服务端推理凭据，不放进 App 或 Git |
| BIRTHDAY_ARK_MODEL_ID | 支持结构化输出的专用模型配置 |
| BIRTHDAY_ARK_TIMEOUT_SECONDS | 默认 70 秒，上限 75 秒 |
| BIRTHDAY_ARK_INPUT_CNY_PER_MILLION_TOKENS | 对应模型输入成本 |
| BIRTHDAY_ARK_OUTPUT_CNY_PER_MILLION_TOKENS | 对应模型输出成本 |
| BIRTHDAY_QUOTA_DAILY_TOKENS | 单设备每日额度 |
| BIRTHDAY_QUOTA_MONTHLY_TOKENS | 单设备每月额度 |
| BIRTHDAY_QUOTA_REQUESTS_PER_MINUTE_DEVICE | 单设备请求频率 |
| BIRTHDAY_QUOTA_MAX_CONCURRENT_DEVICE | 单设备并发限制 |

输入上限 64 KiB，provider 输出读取上限 256 KiB。请求包含随机会话、草稿版本、庆祝日期、已选事实和实际反馈；不使用人物 ID 作为服务账户。模型输出后再次验证数量、来源引用、预算、依赖与明确禁忌，服务器填写请求引用、版本和 `source=model`。

客户端配置 `BIRTHDAY_AI_BASE_URL` 为生日专用 HTTPS 根地址，设备以生日自身的 App Attest 身份注册。模拟器不支持真实 App Attest；`-ui-assistant-fixture` 的结果必须标注为模拟数据，不能作为设备身份或真实推理验收。

## 验证与开通

普通 Go 测试覆盖请求验证、循环依赖、已答问题、预算与引用约束、其他 App 身份拒绝，以及合成 Responses HTTP 编码和返回解析。显式设置 `BIRTHDAY_AI_LIVE_CHECK=1` 后，`go test ./internal/birthday -run TestLiveBirthdayAssistant -count=1 -v` 才执行真实付费模型检查，使用固定合成人物。该检查有 12 个情境和 2 元的本地保守预留上限，输出只含合成内容。实际计费以供应商为准。

真实调用发现并修复了自由类型值、未知预算被写成数字、追问选项为空与禁忌用餐等问题：输出 schema 随请求约束类型、预算和问题数量，校验继续拒绝不符合合同的内容。[Responses API](https://docs.volcengine.com/docs/ark/create-model-responses-api?lang=zh) 的结构化输出仍处于 beta，服务错误或无效结果应明确显示并保留手动路径。`store=false` 不等于零留存承诺，数据处理说明参照[供应商专用条款](https://docs.volcengine.com/docs/ark/volcengine-ark-platform-terms?lang=zh)。

开通需完成：专用主机的 DNS/TLS/路由；真实签名设备注册；合成人物的成功、超时、限额与非法响应；模型计价与额度；供应商数据处理说明；至少十二组产品情境的人工检查。`store=false` 是调用设置，不构成零留存承诺。服务日志只应记录请求引用、版本、耗时与错误，不记录人物内容。

首期服务器不创建计划、不发送通知、不写入偏好。模型输出由客户端展示，用户选择后在设备保存；重试不会在服务端创建第二份计划。准备事项和长期候选喜好由后续模块实现。
