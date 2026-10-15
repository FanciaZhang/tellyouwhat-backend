# 告你系列后端环境

客户端约定：Debug 编译、Ad Hoc 分发、正式 Bundle ID 加 `.dev` 使用测试服务；Release TestFlight 与 App Store 使用正式身份和生产服务。只维护测试与生产两套，不通过客户端自报环境授权。

私有 OTA 服务、限时授权和清理归属见 [Apple 开发版 OTA](apple-development-delivery.md)。独立服务已通过 `install.tellyouwhat.cn` 提供 HTTPS 入口，真实代理故障的三种 Host 日志保护及服务恢复、手动加密异地备份已验证。当前无已发布安装包；原生签名/源码门禁后的交付、到期、定时运行和手机安装仍待验收，不能把传输入口可达当成这些结果。

## 平台 gateway/worker

Health、Journal、Birthday 的 `APP_ENV=development` 默认解析为：

| App | Bundle ID | Host |
| --- | --- | --- |
| Health | cn.tellyouwhat.healthapp.dev | api-test.health.tellyouwhat.cn |
| Journal | cn.tellyouwhat.journalapp.dev | api-test.journal.tellyouwhat.cn |
| Birthday | cn.tellyouwhat.birthday.dev | api-test.birthday.tellyouwhat.cn |

`APP_ENV=production` 保留原正式 Bundle ID 和 `api.<app>.tellyouwhat.cn`。Birthday 继续需要显式启用。每个 App 的显式配置覆盖仍受环境校验约束：开发身份必须以 `.dev` 结尾；正式身份不得以 `.dev` 结尾。已约定的正式 API 域名不得用在开发配置，测试域名不得用在正式配置。localhost 和独立集成测试域名仍可用于本地验证。

MySQL 模式在打开连接前检查：数据库与账号非空、不得使用 root；开发数据库和账号均以 `_test` 结尾；生产两者均不得以 `_test` 结尾。错误不回显 DSN。命名检查只能拦截复制错配置，不能证明 MySQL grants 隔离。部署必须验证测试账号只能访问测试库，Redis/队列、对象存储、加密密钥和备份也须独立。

服务器测试栈配置为 `compose.platform-test.yaml`，独立项目、网络及数据卷，不作为生产 Compose 的 override。使用 `platform_test` 库/账号，Redis 不与生产共享，仅 gateway 的 18790 端口绑定回环地址；数据库、Redis 和 worker 不发布主机端口。健康等平台业务可使用此栈；手记新开发协议仍由其专用服务承接，不能把域名直接接到此 gateway 冒充协议兼容。

启动前提供 `PLATFORM_TEST_ENV_FILE`（受限运行配置）、`PLATFORM_TEST_IMAGE_TAG`（准确提交）、三个独立的 `PLATFORM_TEST_MYSQL_PASSWORD` / `PLATFORM_TEST_MYSQL_ROOT_PASSWORD` / `PLATFORM_TEST_REDIS_PASSWORD`。运行配置须另行生成加密/内部授权/开发激活密钥，配置真实 `.dev` 身份与构建白名单、开发对象存储及供应商凭据，不复制生产运行配置。发布使用 `platform-test` 清理归属，保护当前/前一版镜像和配置；数据库授权、备份、模型请求及公网 HTTPS 验收通过后再切换测试域名。

实际主机 Compose 解析器已用合成配置验证独立资源和端口限制，并验证缺少五项必要配置时拒绝；清理/注册等相关 42 项运维测试通过。见 `evidence/platform-test-configuration-2026-10-15.json`。当前未部署此栈、创建运行凭据或切换任何路由。

测试入口由 `deploy/tencent/test_routes.py --enable <app> --journal-voice-protocol <当前客户端版本>` 管理。脚本按固定测试域名/回环端口先检查实际容器身份、运行环境与 API 合约，再在运维/部署双锁下验证和激活独立 Caddy 片段；生产路由在切换前后均按当前 slot 核对。失败恢复之前的根配置与片段，未管理的片段拒绝覆盖。手记 readiness 使用现有开发令牌，禁止携带令牌跟随重定向。DNS 就绪后才执行入口切换；本地回归不代替 TLS 或真实业务验收。

根目录 `.env.example` 与 `compose.yaml` 现在创建 `tellyouwhat_test` 库及账号，使用新的测试数据卷。已有旧本地卷保持原样；不会自动搬迁或清空。迁移前先确认是否需要保留其中的数据。共享 `cmd/migrate` 仍供平台与相册使用，本次没有把平台运行时命名检查冒充迁移器的权限保证；迁移必须由绑定环境的部署流程和受限账号执行。

`APP_ATTEST_ENV` 在两套 API 环境中均默认 production，与同一 `.dev` 身份的 Xcode 直装和 Ad Hoc 配置一致。设备证明仍校验实际 Bundle ID 和证明环境。注册时是否要求开发激活口令由 API 环境独立决定；开发服务同时保留构建白名单，首次注册及注册重试均执行访问校验。平台开发权益按 API 环境开通；正式服务继续验证 Apple 交易并区分 Sandbox/Production。已有部署的显式 development 配置不会被默认值自动切换，迁移需核对客户端和已存设备密钥，保留正式数据。

## 相册推送

相册推送登记使用最终安装包的 APNs 环境，独立于 API 环境、Debug 编译条件和 StoreKit。测试服务默认 `ALBUM_PUSH_ENVIRONMENT=production` 供 Ad Hoc 使用；需要同时支持 Xcode Development 安装时，显式开启 `ALBUM_PUSH_ALLOW_DEVELOPMENT=true`，并分别提供两种 APNs 环境的签名 key。正式部署拒绝 development 默认值和双通道开关。

`X-Album-Push-Environment` 只是通道路由信息，不能授权访问、指定 topic 或选择其他服务。服务端仍使用登录会话确定账号。切换同一会话的签名通道时，旧注册增加 revision 并清除令牌；已被 APNs 接受的提醒按会话去重，避免重签安装后重复发送。多设备、不同通道的待发送项全部处理后才结算提醒。原有表已保存环境，不需要新增迁移。旧客户端省略 header 时沿用服务端默认值。

## 集成来源

统一分支从共享主干 `da7c981` 开始，合并相册 `79ba5db` 与生日 `e581c28`。保留主干最新手记业务、清理操作用户检查，以及相册就绪检查协议。迁移按完整文件名记账，历史 SQL 不重命名。

## 仍需完成

- 相册独立 gateway 已将 Apple 登录/兑换/撤销、账号通知、账号哈希、APNs topic 和 StoreKit 验证绑定到部署身份：test 使用 `cn.tellyouwhat.albums.dev`，production 使用原正式 ID。正式账号哈希与 StoreKit token 命名空间保持兼容。新开发栈须使用新库，不能原地把已有正式身份的测试库切成 `.dev`；真实权限及已有数据升级仍待验证。
- 相册生产订阅服务已分别验证正式身份的 Production/Sandbox 交易与通知，后台按购买环境独立刷新。有正式购买历史时始终使用正式容量、验证有效期及保留期；仅沙箱购买的账号默认限制为 1 GB（`ALBUM_SANDBOX_QUOTA_BYTES`），沙箱加速到期停止新上传但不产生清理期限，已有原件保持短时读授权。签名路由、真实 MySQL 共存/容量/下载已验证；真实 TestFlight 购买和双环境定期订阅刷新运行验收仍待完成。
- 跨购买环境的订阅改绑与在线账号记录擦除已接入：最后一条确认会通过各自 Apple 环境重新核验全部购买链，证明绑定购买环境、交易版本和共享 revision。另一侧失败、未改绑、新发现购买链或并发修改均阻止擦除；单环境服务仍拒绝处理另一侧记录。配置允许显式启用，部署默认关闭。真实 MySQL 上的双队列后台改绑、记录擦除及并发确认保护已通过竞态检测；Apple 状态和媒体清理采用测试替身，这不代表真实 Apple 注销、备份清理和真机旅程已验收。
- `.dev` 不继承正式商品目录；显式开发容量实现与验证见下节，真实测试部署和手机流程仍待完成，不能拿正式购买凭据绕过身份绑定。
- Journal 私有开发服务有独立的开发连接与语音协议；新测试域名需接通现有客户端合约，不能直接替换为不兼容的共享 gateway。
- Birthday 开发版和生产候选的旧路径需迁移，保持现有客户端过渡兼容。
- 创建并核查独立测试账号、资源及服务，配置公网 HTTPS/DNS，注册清理/备份责任，再验收真机 Ad Hoc 到测试 API 的完整请求。
- 相册现有正式样式域名仍需从测试栈迁出。Release 不得因此被当作已接通生产。
- Sandbox 权益和真实用户数据清理隔离须按业务实测；仅配置校验通过不算验收。

本地 Go 单元测试通过不代表真实 MySQL、云权限、Apple 身份、外网路由或生产部署通过。本轮未修改远端服务。

## 已启用的手记测试入口

`api-test.journal.tellyouwhat.cn` 已通过 Netlify CLI 创建 A 记录并核对服务器解析；Caddy 独立测试片段已启用，切换前后的生产 slot 路由均验证通过。现有兼容私有服务继续运行，没有重新构建或替换 Go 业务二进制。当前客户端开发凭据通过公网 HTTPS 验证，协议为 `subscription-v2` / `journal-voice-v36`；合成日记真实模型整理成功，未授权/错误凭据返回 401，内部路径返回 404。首次模型探针使用不同的请求头/正文 ID，被服务器拒绝；修正探针后通过。

路由源码 `f41ac04`，配置备份增强 `d6b5966` 已安装到服务器，相关 32 项测试通过。一次实际加密备份包含 Caddy 根配置、测试片段和路由状态，逐文件摘要/认证清单验证通过，COS 三个对象均已读回核对。此结果不代表测试数据库、手记状态或真实媒体备份已完成，也不是新定时运行证据。手记独立 Prompt 数据源、状态备份、真实语音、手机安装与 CloudKit 仍待验收。证据为 `evidence/journal-test-route-activation-2026-10-15.json`。

## 相册容量响应契约

生产双购买环境返回可选 `quotaKind`：有正式购买历史时为 `subscription`，仅沙箱购买时为 `sandbox`。旧响应省略字段仍按正式套餐规则校验。后者允许配置上限内的测试容量，且有效/到期状态都不产生自动清理期限。客户端必须同步支持该字段，不能继续要求沙箱容量等于 50/500 GB 或必须带六个月期限。后端源码 `7771c17` 已更新 OpenAPI/生成模型及策略标记，包测试和策略竞态检查通过；未部署，未代替真实 Apple 购买或 MySQL 条件测试。证据为 `evidence/albums-sandbox-quota-contract-2026-10-15.json`。独立 `.dev` 测试容量授权仍待实现。

## 独立开发版云容量

`ALBUM_DEVELOPMENT_CLOUD_QUOTA_BYTES`、`ALBUM_DEVELOPMENT_CLOUD_ACCOUNTS`、`ALBUM_DEVELOPMENT_CLOUD_EXPIRES_AT` 必须一起设置；默认全部为空，不授权容量。只允许 `test` 部署、开发 audience、测试库及以 `_test` 结尾的数据库账号，最多三个规范账户 UUID，每账户最多 1 GB，并明确 RFC3339 到期时间。它与 StoreKit 开通互斥，不创建购买记录或修改套餐容量，不接受客户端自报额度。

Gateway 与 worker 读取相同配置，按现有数据库锁检查已用/预留容量。返回 `quotaKind=development`、无商品 ID、`purchasesEnabled=false`。到期停止新增媒体；已上传内容由账户和版本归属检查后发放最多五分钟的下载/分享授权。开发授权不产生清理期限，并拒绝自动到期清理证明。删除整个测试部署配置属于运维停用，不能代替用户的数据导出或删除流程。

真实 MySQL 上验证了零购买记录下的上传预留、worker 验证结算、并发额度、未授权账户拒绝及到期原件读取；使用合成账户和 COS 替身，不代表真实备份或 PhotoKit 删除验收。两个新数据库场景和一个推送回归场景均通过竞态检查，生成的库/账号均清理。全量 Go 测试 56 个包通过，13 个包无测试；最终生成契约后相关包再次通过。实际环境尚未启用开发授权。
