# 告你系列后端环境

客户端约定：Debug 编译、Ad Hoc 分发、正式 Bundle ID 加 `.dev` 使用测试服务；Release TestFlight 与 App Store 使用正式身份和生产服务。只维护测试与生产两套，不通过客户端自报环境授权。

## 平台 gateway/worker

Health、Journal、Birthday 的 `APP_ENV=development` 默认解析为：

| App | Bundle ID | Host |
| --- | --- | --- |
| Health | cn.tellyouwhat.healthapp.dev | api-test.health.tellyouwhat.cn |
| Journal | cn.tellyouwhat.journalapp.dev | api-test.journal.tellyouwhat.cn |
| Birthday | cn.tellyouwhat.birthday.dev | api-test.birthday.tellyouwhat.cn |

`APP_ENV=production` 保留原正式 Bundle ID 和 `api.<app>.tellyouwhat.cn`。Birthday 继续需要显式启用。每个 App 的显式配置覆盖仍受环境校验约束：开发身份必须以 `.dev` 结尾；正式身份不得以 `.dev` 结尾。已约定的正式 API 域名不得用在开发配置，测试域名不得用在正式配置。localhost 和独立集成测试域名仍可用于本地验证。

MySQL 模式在打开连接前检查：数据库与账号非空、不得使用 root；开发数据库和账号均以 `_test` 结尾；生产两者均不得以 `_test` 结尾。错误不回显 DSN。命名检查只能拦截复制错配置，不能证明 MySQL grants 隔离。部署必须验证测试账号只能访问测试库，Redis/队列、对象存储、加密密钥和备份也须独立。

根目录 `.env.example` 与 `compose.yaml` 现在创建 `tellyouwhat_test` 库及账号，使用新的测试数据卷。已有旧本地卷保持原样；不会自动搬迁或清空。迁移前先确认是否需要保留其中的数据。共享 `cmd/migrate` 仍供平台与相册使用，本次没有把平台运行时命名检查冒充迁移器的权限保证；迁移必须由绑定环境的部署流程和受限账号执行。

`APP_ATTEST_ENV` 与 API 环境独立。开发身份的 Ad Hoc 产物可以需要 production App Attest，不能仅因 Apple 认证环境是 production 就连接生产 API。平台开发权益按 API 环境开通；正式服务继续验证 Apple 交易并区分 Sandbox/Production。

## 集成来源

统一分支从共享主干 `da7c981` 开始，合并相册 `79ba5db` 与生日 `e581c28`。保留主干最新手记业务、清理操作用户检查，以及相册就绪检查协议。迁移按完整文件名记账，历史 SQL 不重命名。

## 仍需完成

- 相册是独立 gateway/worker，现有 Apple 登录、通知、推送等仍硬编码正式 audience；需完整改为服务端绑定的开发/正式身份，保留已有账号哈希与购买关联兼容，不能只改域名。
- Journal 私有开发服务有独立的开发连接与语音协议；新测试域名需接通现有客户端合约，不能直接替换为不兼容的共享 gateway。
- Birthday 开发版和生产候选的旧路径需迁移，保持现有客户端过渡兼容。
- 创建并核查独立测试账号、资源及服务，配置公网 HTTPS/DNS，注册清理/备份责任，再验收真机 Ad Hoc 到测试 API 的完整请求。
- 相册现有正式样式域名仍需从测试栈迁出。Release 不得因此被当作已接通生产。
- Sandbox 权益和真实用户数据清理隔离须按业务实测；仅配置校验通过不算验收。

本地 Go 单元测试通过不代表真实 MySQL、云权限、Apple 身份、外网路由或生产部署通过。本轮未修改远端服务。
