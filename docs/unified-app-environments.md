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

- 相册独立 gateway 已将 Apple 登录/兑换/撤销、账号通知、账号哈希、APNs topic 和 StoreKit 验证绑定到部署身份：test 使用 `cn.tellyouwhat.albums.dev`，production 使用原正式 ID。正式账号哈希与 StoreKit token 命名空间保持兼容。新开发栈须使用新库，不能原地把已有正式身份的测试库切成 `.dev`；真实权限及已有数据升级仍待验证。
- 相册生产订阅服务已分别验证正式身份的 Production/Sandbox 交易与通知，后台按购买环境独立刷新。有正式购买历史时始终使用正式容量、验证有效期及保留期；仅沙箱购买的账号默认限制为 1 GB（`ALBUM_SANDBOX_QUOTA_BYTES`），沙箱加速到期停止新上传但不产生清理期限，已有原件保持短时读授权。签名路由、真实 MySQL 共存/容量/下载已验证；真实 TestFlight 购买和双环境定期订阅刷新运行验收仍待完成。
- 跨购买环境的订阅改绑与在线账号记录擦除已接入：最后一条确认会通过各自 Apple 环境重新核验全部购买链，证明绑定购买环境、交易版本和共享 revision。另一侧失败、未改绑、新发现购买链或并发修改均阻止擦除；单环境服务仍拒绝处理另一侧记录。配置允许显式启用，部署默认关闭。真实 MySQL 上的双队列后台改绑、记录擦除及并发确认保护已通过竞态检测；Apple 状态和媒体清理采用测试替身，这不代表真实 Apple 注销、备份清理和真机旅程已验收。
- `.dev` 不继承正式商品目录；独立开发测试权益及客户端测试容量说明仍待完成，不能拿正式购买凭据绕过身份绑定。
- Journal 私有开发服务有独立的开发连接与语音协议；新测试域名需接通现有客户端合约，不能直接替换为不兼容的共享 gateway。
- Birthday 开发版和生产候选的旧路径需迁移，保持现有客户端过渡兼容。
- 创建并核查独立测试账号、资源及服务，配置公网 HTTPS/DNS，注册清理/备份责任，再验收真机 Ad Hoc 到测试 API 的完整请求。
- 相册现有正式样式域名仍需从测试栈迁出。Release 不得因此被当作已接通生产。
- Sandbox 权益和真实用户数据清理隔离须按业务实测；仅配置校验通过不算验收。

本地 Go 单元测试通过不代表真实 MySQL、云权限、Apple 身份、外网路由或生产部署通过。本轮未修改远端服务。
