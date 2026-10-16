# 告你系列后端环境

日常开发使用 `.dev` 身份、Debug 编译、Ad Hoc OTA 和测试 API；TestFlight 与 App Store 使用正式身份、Release 和生产 API。只维护测试、生产两套，不以客户端自报环境授权。

仅健康已上线，需要保留正式用户升级与数据兼容。相册、手记、生日直接迁到目标身份、接口和存储模型，不新增旧开发包适配或过渡路由；已有数据、备份和用户未提交源码仍保留。旧服务退役与不可恢复的数据删除是两件事。

## 当前服务

| App | 开发身份 | 测试 API | 当前状态 |
| --- | --- | --- | --- |
| 健康 | cn.tellyouwhat.healthapp.dev | api-test.health.tellyouwhat.cn | 开发包已交付，独立测试存储与服务尚未完成 |
| 手记 | cn.tellyouwhat.journalapp.dev | api-test.journal.tellyouwhat.cn | 独立开发服务 18787 已接通；Prompt 已脱离正式库，配置与费用账本独立加密备份；真实语音、供应商身份和 CloudKit 待验收 |
| 生日 | cn.tellyouwhat.birthday.dev | api-test.birthday.tellyouwhat.cn | 独立测试网关 18790、MySQL/Redis 及备份已接通；真实手机注册/推理待完成 |
| 相册 | cn.tellyouwhat.albums.dev | api-test.albums.tellyouwhat.cn | 独立 API/worker 18092 及备份已接通；真实登录、容量与媒体旅程待完成 |

正式身份为对应 Bundle ID 去掉 `.dev`，目标域名为 `api.<app>.tellyouwhat.cn`。现有健康/手记正式服务仍为 18180；正式相册域名仍指向旧测试栈，不能据域名宣称已具备生产环境。生日正式域名及推理身份仍需迁移。未上线 App 的旧路径不作为兼容交付要求。

`install.tellyouwhat.cn` 已交付四个 App 的签名开发包，安装服务实际回滚并恢复当前版通过。生日最新为源码 `3354a9a`、1.0 (4)，已取消手输激活码，首次使用自动验证设备；测试后端 `fb5cd0b` 保留 App Attest、构建白名单及额度限制。详见 [OTA 与验收边界](apple-development-delivery.md)及 `evidence/birthday-automatic-enrollment-ota-2026-10-16.json`。

## 隔离和部署

平台 `APP_ENV=development` 默认使用 `.dev` 与 `api-test.*`，production 使用正式身份和 `api.*`。配置相反的身份、域名或数据库时，连接数据库前拒绝启动；开发库和账号均以 `_test` 结尾，生产两者均不得以该后缀结尾，且均不能使用 root。实际 MySQL grants、Redis、对象存储及会话/载荷密钥仍须分别验证，命名检查不能代替权限隔离。

`compose.platform-test.yaml` 是独立项目、网络和数据卷，默认子网 `172.29.93.0/24`，仅 gateway 的 18790 端口发布到回环地址。当前源码 `fb5cd0b` 只启用生日，66 项迁移完成；健康须先准备自己的测试媒体资源再接入。生日开发推理身份仍被旧 TestFlight 服务使用，正式推理资源隔离尚未完成。

待部署源码 `7c0e6d2` 使用 `PLATFORM_APPS` 显式选择 App，只读取所选 App 的身份和提供商配置。健康接入要求 `health,birthday`、独立 `health-ai-test` 私有桶、测试模型端点与独立凭据；部署前逐项拒绝与当前正式配置相同的凭据或端点。更新保留原 MySQL、Redis、网络、加密密钥和 schema，先验证异地备份，再启动 gateway/worker；同时检查两种 Host 和 worker 的实际运行状态，失败恢复匹配的旧配置。旧 source/configuration 只为已登记备份及回滚校验保留，不提供旧开发客户端适配。

健康媒体 IAM 策略已准备在 [health-development-media-only.json](../config/iam/health-development-media-only.json)，仅允许 `health-ai-test/ai-temp/health/*` 的上传、读取和删除对象；尚未在云端创建或绑定。需要完成控制台登录、独立模型与身份配置，再实际检查本前缀可读写、其他前缀与正式桶拒绝；名称和配置不作为权限验收证据。

生产发布使用 `release.py deploy`；测试和 OTA 使用已登记的产物所有者入口，由 ubuntu 执行，锁定准确源码、镜像与匹配配置。不得通过 root wrapper、移动镜像标签或独立迁移替代既有流程。只保留恢复所需的当前/上一版，不要求未上线 App 继续运行旧协议。见 [产物与备份责任](host-artifact-lifecycle.md)。

测试路由由 `test_routes.py` 管理，当前启用 albums、birthday、journal。切换前核对实际身份/合约，验证完整 Caddy 配置；切换后检查正式 slot 和 OTA 日志保护。真实 DNS/TLS、鉴权及完整业务旅程分别取证。

## 备份

统一运维任务和私有 COS 传输复用现有设施，测试数据库使用独立 SELECT 账号、备份目录和 SQL 解密密钥，App 不持有运维备份凭据。内层密钥保存在受限配置和独立 Mac 留存位置，不进入同一恢复包。

- 相册备份包含完整 36 张业务表及删除账本；COS 实际取回、Mac 临时 MySQL 的行数/内容摘要及恢复隔离通过。
- 平台测试备份沿用本地优先控制记录白名单：COS 实际取回，临时 MySQL 恢复 93 张表、73 条保留记录，行数一致；设备身份、购买、AI 请求及媒体行不恢复。临时容器已清理。
- 手记测试服务不再持有数据库凭据，Prompt 为独立修订文件。配置、匹配程序及 8,905 条费用事件使用独立密钥加密；COS 实取三对象、断网容器中真实程序的 Prompt 校验/账本重放、Mac 独立密钥与错误密钥拒绝均通过。不备份录音、图片任务、旧同意或连接令牌；恢复需新建连接。
- OTA 源码绑定配置、二进制和代理同属认证恢复清单；安装包及短时授权可再生，不作业务备份。

以上是手动检查，不代表新路径已通过真实定时任务验收。证据：`evidence/platform-test-runtime-2026-10-16.json`、`evidence/albums-development-cos-restore-2026-10-16.json`、`evidence/apple-delivery-native-rollback-2026-10-16.json`。

## Apple 与权益

平台 App Attest 两套 API 均使用 production 证明环境，仍严格核对各自 Bundle ID；开发注册自动验证设备与允许构建号，无需手输口令。生日现网已采用此方式，健康源码已完成但测试服务尚未接入。开发权益限制 App、设备和最多 30 天期限，不复制交易标识或创建购买记录；写入与真实购买升级使用同一行锁，拒绝覆盖 production/sandbox 权益。正式服务不配置开发授权入口。手记私有测试服务使用独立连接凭据，Release 不接受其模拟权益声明。StoreKit Sandbox 与 API 环境独立，正式候选在生产 API 验证 Sandbox/Production 交易，正式购买优先，沙箱加速到期不触发正式数据清理。

相册 `.dev` 不继承正式商品目录，只接受服务端明确账户、额度和期限的开发授权：最多三个指定账户、每账户不超过 1 GB，不创建购买记录，到期停写但不生成清理期限。当前开发服务尚未配置真实账户容量；真实登录后再授予。签名检查、受控 Apple/MySQL 测试不能代替真实购买和手机旅程。

相册推送按最终签名的 APNs 环境路由，独立于 Debug/API/StoreKit；双通道只允许独立测试身份，正式服务拒绝 development 通道。实际 key、收件与所有删除功能仍需单独验收；细节见 [相册部署说明](../deploy/albums/README.md)。

## 剩余工作

健康独立测试资源与真实云同步；手记独立供应商身份、真实语音与 CloudKit；生日正式 API/推理资源与日志导出；相册正式资源、真实登录/容量及媒体链路；四个 App 的手机安装、真实权益和普通 Release TestFlight 候选。健康继续验证正式用户升级，其他 App 不再增加历史开发包兼容门禁。

手记隔离证据：`evidence/journal-development-isolation-2026-10-16.json`，后端源码 `29b832c`。旧正式库只读凭据已撤销，所有运行/停止容器均无依赖，旧配置及临时恢复资源已清理。合成日记通过真实模型整理，响应使用独立 Prompt 修订且请求/内容绑定正确；这不代表手机语音、编辑保存或 CloudKit 已通过。供应商权限身份仍待独立，文件分开不作为云端 IAM 隔离证据。

健康自动开发认证证据：`evidence/health-automatic-development-access-2026-10-16.json`。客户端 69 项定向测试、普通 Release 编译及真实产物检查通过；后端全量 Go 测试、178 项运维 Python 检查（1 项跳过）、3 项独立真实 MySQL 集成检查通过。正式健康网关仍为原镜像、未重启；这批源码尚未部署健康测试 API，也未导出新开发安装包。
