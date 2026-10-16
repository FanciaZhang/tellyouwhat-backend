# 告你系列后端环境

日常开发使用 `.dev` 身份、Debug 编译、Ad Hoc OTA 和测试 API；TestFlight 与 App Store 使用正式身份、Release 和生产 API。只维护测试、生产两套，不以客户端自报环境授权。

仅健康已上线，需要保留正式用户升级与数据兼容。相册、手记、生日直接迁到目标身份、接口和存储模型，不新增旧开发包适配或过渡路由；已有数据、备份和用户未提交源码仍保留。旧服务退役与不可恢复的数据删除是两件事。

## 当前服务

| App | 开发身份 | 测试 API | 当前状态 |
| --- | --- | --- | --- |
| 健康 | cn.tellyouwhat.healthapp.dev | api-test.health.tellyouwhat.cn | 独立测试 API、worker、受限云身份与 HTTPS 已接通；新开发包、真实 App Attest 和云同步待验收 |
| 手记 | cn.tellyouwhat.journalapp.dev | api-test.journal.tellyouwhat.cn | 独立开发服务、Prompt、推理及语音身份已接通；配置与费用账本独立加密备份；手机业务旅程和 CloudKit 待验收 |
| 生日 | cn.tellyouwhat.birthday.dev | api-test.birthday.tellyouwhat.cn | 独立测试网关 18790、MySQL/Redis 及备份已接通，开发包 1.0 (5) 支持诊断导出；真实手机注册/推理待完成 |
| 相册 | cn.tellyouwhat.albums.dev | api-test.albums.tellyouwhat.cn | 独立 API/worker 18092 及备份已接通；真实登录、容量与媒体旅程待完成 |

正式身份为对应 Bundle ID 去掉 `.dev`，目标域名为 `api.<app>.tellyouwhat.cn`。现有健康/手记正式服务仍为 18180；正式相册域名仍指向旧测试栈，不能据域名宣称已具备生产环境。生日正式域名及推理身份仍需迁移。未上线 App 的旧路径不作为兼容交付要求。

`install.tellyouwhat.cn` 已交付四个 App 的签名开发包，安装服务实际回滚并恢复当前版通过。生日最新为源码 `398b2ff`、1.0 (5)，自动验证设备并支持 Debug 诊断导出；测试后端 `7c0e6d2` 保留 App Attest、构建白名单及额度限制。详见 [OTA 与验收边界](apple-development-delivery.md)及 `evidence/birthday-automatic-enrollment-ota-2026-10-16.json`。

## 隔离和部署

平台 `APP_ENV=development` 默认使用 `.dev` 与 `api-test.*`，production 使用正式身份和 `api.*`。配置相反的身份、域名或数据库时，连接数据库前拒绝启动；开发库和账号均以 `_test` 结尾，生产两者均不得以该后缀结尾，且均不能使用 root。实际 MySQL grants、Redis、对象存储及会话/载荷密钥仍须分别验证，命名检查不能代替权限隔离。

`compose.platform-test.yaml` 是独立项目、网络和数据卷，默认子网 `172.29.93.0/24`，仅 gateway 的 18790 端口发布到回环地址。当前源码 `761dc7b` 启用健康和生日，66 项迁移保持不变；健康使用独立测试桶、三个测试端点及受限身份。生日开发推理身份仍被旧 TestFlight 服务使用，正式推理资源隔离尚未完成。

源码 `761dc7b` 已以 `PLATFORM_APPS=health,birthday` 部署，生日允许构建 4、5，健康允许构建 4；只初始化已启用 App 的 Prompt，健康不会依赖手记模型。健康使用独立 `health-ai-test` 私有桶、测试模型端点与独立凭据；部署前逐项拒绝与当前正式配置相同的凭据或端点。更新保留原 MySQL、Redis、网络、加密密钥和 schema，先验证异地备份，再启动所需 gateway/worker；检查所选 Host 和 worker 的实际运行状态，失败恢复匹配的旧配置。旧 source/configuration 只为已登记备份及回滚校验保留，不提供旧开发客户端适配。

健康媒体 IAM 策略已准备在 [health-development-media-only.json](../config/iam/health-development-media-only.json)，仅允许 `health-ai-test/ai-temp/health/*` 的上传、读取和删除对象；已创建并绑定，实测本前缀可读写删除，其他前缀与正式桶拒绝。推理策略在 `health-development-inference-only.json`，仅包含三个测试端点；名称和配置不作为权限验收证据。

生产发布使用 `release.py deploy`；测试和 OTA 使用已登记的产物所有者入口，由 ubuntu 执行，锁定准确源码、镜像与匹配配置。不得通过 root wrapper、移动镜像标签或独立迁移替代既有流程。只保留恢复所需的当前/上一版，不要求未上线 App 继续运行旧协议。见 [产物与备份责任](host-artifact-lifecycle.md)。

测试路由由 `test_routes.py` 管理，当前启用 albums、birthday、health、journal。切换前核对实际身份/合约，验证完整 Caddy 配置；切换后检查正式 slot 和 OTA 日志保护。真实 DNS/TLS、鉴权及完整业务旅程分别取证。

## 备份

统一运维任务和私有 COS 传输复用现有设施，测试数据库使用独立 SELECT 账号、备份目录和 SQL 解密密钥，App 不持有运维备份凭据。内层密钥保存在受限配置和独立 Mac 留存位置，不进入同一恢复包。

- 相册备份包含完整 36 张业务表及删除账本；COS 实际取回、Mac 临时 MySQL 的行数/内容摘要及恢复隔离通过。
- 平台测试备份沿用本地优先控制记录白名单：COS 实际取回，临时 MySQL 恢复 93 张表、73 条保留记录，行数一致；设备身份、购买、AI 请求及媒体行不恢复。临时容器已清理。
- 手记测试服务不再持有数据库凭据，Prompt 为独立修订文件。配置、匹配程序及 8,905 条费用事件使用独立密钥加密；COS 实取三对象、断网容器中真实程序的 Prompt 校验/账本重放、Mac 独立密钥与错误密钥拒绝均通过。不备份录音、图片任务、旧同意或连接令牌；恢复需新建连接。
- OTA 源码绑定配置、二进制和代理同属认证恢复清单；安装包及短时授权可再生，不作业务备份。

以上是手动检查，不代表新路径已通过真实定时任务验收。证据：`evidence/platform-test-runtime-2026-10-16.json`、`evidence/albums-development-cos-restore-2026-10-16.json`、`evidence/apple-delivery-native-rollback-2026-10-16.json`。

## Apple 与权益

平台 App Attest 两套 API 均使用 production 证明环境，仍严格核对各自 Bundle ID；开发注册自动验证设备与允许构建号，无需手输口令。生日和健康测试服务均采用此方式；健康新签名包已交付，真实手机证明尚未验收。开发权益限制 App、设备和最多 30 天期限，不复制交易标识或创建购买记录；写入与真实购买升级使用同一行锁，拒绝覆盖 production/sandbox 权益。正式服务不配置开发授权入口。手记私有测试服务使用独立连接凭据，Release 不接受其模拟权益声明。StoreKit Sandbox 与 API 环境独立，正式候选在生产 API 验证 Sandbox/Production 交易，正式购买优先，沙箱加速到期不触发正式数据清理。

相册 `.dev` 不继承正式商品目录，只接受服务端明确账户、额度和期限的开发授权：最多三个指定账户、每账户不超过 1 GB，不创建购买记录，到期停写但不生成清理期限。当前开发服务尚未配置真实账户容量；真实登录后再授予。签名检查、受控 Apple/MySQL 测试不能代替真实购买和手机旅程。

相册推送按最终签名的 APNs 环境路由，独立于 Debug/API/StoreKit；双通道只允许独立测试身份，正式服务拒绝 development 通道。实际 key、收件与所有删除功能仍需单独验收；细节见 [相册部署说明](../deploy/albums/README.md)。

## 剩余工作

健康真实手机证明与云同步；手记手机录音/保存与 CloudKit；生日正式 API/推理资源；相册正式资源、真实登录/容量及媒体链路；四个 App 的手机安装、真实权益和普通 Release TestFlight 候选。健康继续验证正式用户升级，其他 App 不再增加历史开发包兼容门禁。

手记隔离证据：`evidence/journal-development-isolation-2026-10-16.json`，后端源码 `29b832c`。旧正式库只读凭据已撤销，所有运行/停止容器均无依赖，旧配置及临时恢复资源已清理。合成日记通过真实模型整理，响应使用独立 Prompt 修订且请求/内容绑定正确；这不代表手机语音、编辑保存或 CloudKit 已通过。供应商权限身份仍待独立，文件分开不作为云端 IAM 隔离证据。

手记客户端 `f238cd8` 排除普通 Release 的持久化、iCloud 和模拟商品目录启动覆盖，Debug 22 项、Release 20 项通过，实际新建内容在旧参数下重启仍保留。另分别跳过 2、4 项真实 Apple 或渠道限定检查；未改模型或正式容器，未推送或更新 OTA。见 `evidence/journal-release-runtime-boundary-2026-10-16.json`。生日契约 `d55b2d3` 已改为生产/测试独立域名，三包定向测试和 `make verify-generated` 通过；此契约元数据更新未部署服务。

健康自动开发认证证据：`evidence/health-automatic-development-access-2026-10-16.json`。客户端 69 项定向测试、普通 Release 编译及真实产物检查通过；后端全量 Go 测试、178 项运维 Python 检查（1 项跳过）、3 项独立真实 MySQL 集成检查通过。正式健康网关仍为原镜像、未重启；这批源码尚未部署健康测试 API，也未导出新开发安装包。

生日远程诊断已交付 `398b2ff`、1.0 (5)：Debug 可导出本次进程的受限 AI 元数据，Release 无入口或实现；56 项 Debug 检查、1 项 Release 界面检查及 27 项核心测试通过，另跳过 1 项真实服务测试。原生双目标签名及公网完整 IPA 摘要、HEAD/Range 通过；发布就绪路径误用 `/healthz` 后，改用实际 `/readyz` 完成 owner 检查和清理，复用原回执，未重复发布。平台测试更新到 `7c0e6d2` 后，MySQL、Redis 和正式网关未重启，66 项 schema 未变，匹配的新加密备份已认证。见 `evidence/birthday-remote-diagnostics-ota-2026-10-16.json`；实际手机安装/注册与推理仍未验收。

相册正式桶已创建（2026-10-16）：`tellyouwhat-albums-production-1251995834` / `ap-guangzhou`，私有、多 AZ、版本控制开启。`albums/originals/` 当前/历史版本 30 天转多 AZ 低频已由控制台保存并读回，无对象年龄删除规则。既有开发桶不迁作正式桶；正式 CAM/CI、部署、路由和真实媒体旅程未就绪，禁止将新桶存在等同正式服务可用。证据：`evidence/albums-production-storage-2026-10-16.json`。

## 健康引导测试入口隔离（2026-10-16）

源码 `8c3df1fd` 将 `UI_TEST_GUIDE_PAGE` 的读取限制为 Debug；正式版继续按真实初始页面决定是否推迟引导。已核对受测源码摘要：iPhone 17 / iOS 26.5 上 35 项单元测试、2 项 UI 测试通过，普通 iOS Release 编译通过，发布二进制不含该开关。另以原样策略源码在 macOS 验证了 6 项非 Debug 断言。未执行新的签名、OTA 或完整发布门禁；截图尚待用户确认。见 `evidence/health-guide-runtime-boundary-2026-10-16.json`。

## 健康独立开发 CloudKit schema（2026-10-16）

独立容器 `iCloud.cn.tellyouwhat.healthapp.dev` 已导入当前同步代码所需的 `AppObjectV2`、`AppMediaV2`，并部署到该开发容器自己的 Production 环境，供 Ad Hoc 包使用。控制台读回确认记录类型、字段及 `manifest` 加密类型；既有 Users 类型不变，未操作业务记录，也未修改正式容器。此证据仅证明 schema 就绪，真实手机写入、恢复、跨设备同步及正式容器隔离仍待验收。见 `evidence/health-development-cloudkit-schema-2026-10-16.json`；可复现 schema 保存在健康仓库 `docs/ci/health-development.ckdb`。

## 相册后台任务数据库隔离（2026-10-16）

源码 `15a5b31` 将 API 既有数据库/环境检查移到与 worker 共用的配置入口，阻止独立 worker 在错误环境连接数据库。28 组配置覆盖测试/生产互串、其他 App 数据库、缺失环境；另外验证 root、空用户和错误 DSN 的拒绝及日志脱敏。相关三个 Go 包共 235 项顶层测试通过、220 项真实集成条件检查跳过。现有 Compose 已给两个进程注入相同配置，新增回归断言；未更新远端服务。见 `evidence/albums-worker-environment-2026-10-16.json`。

## 手记独立开发 CloudKit schema（2026-10-16）

`iCloud.cn.tellyouwhat.journalapp.dev` 的 Development 原先只有 Users。现已从现有手记 Development 结构核对当前源码的 14 种模型、字段和索引，导入独立开发容器并部署到该容器的 Production，供 Ad Hoc 使用。完整 Production schema 导出与导入内容一致；新业务类型仅保留 creator 写权限，未复制公共数据库的读取/创建授权，Users 不变。正式容器及用户记录未改动，没有 App 构建输入变化或新签名包。真实手机上传、下载、跨设备同步和容器隔离仍待验收。见 `evidence/journal-development-cloudkit-schema-2026-10-16.json`；可复现结构在手记仓库 `docs/journal-development.ckdb`。

## 健康测试云资源（2026-10-16）

`health-ai-test` 已创建并读回：北京、标准单 AZ、私有桶与默认私有对象、SSE-TOS；未启用版本控制或生命周期删除。独立 mini 260428、lite 260428、pro 2.1 / 260628 三个按 Token 付费端点已创建，管理 API 均返回 Running，八项功能已按模型复用三个端点接入测试 API。正式图片识别的 pro 2.0 / 260215 已禁止新建，测试改用 2.1 / 260628；正式端点未改，测试效果和结构化输出尚待验收。

`healthapp-development-backend` 尚未创建；媒体前缀和三个端点的最小权限策略已准备，未授予权限、生成凭据或接通健康测试 API。下一步为创建受限身份、验证正式资源访问被拒绝，再通过现有平台测试部署入口接入。正式健康运行镜像和 2026-10-13 启动时间未变。证据：`evidence/health-development-cloud-resources-2026-10-16.json`。

## 健康测试身份与相册测试升级（2026-10-16）

健康 `healthapp-development-backend`（97255372）已创建，只绑定测试媒体前缀 CRUD 与三个指定测试端点策略，无控制台登录或自行管理密钥。AK 与推理 Key 已存于本机和服务器的获准私有目录，SHA-256 读回一致。合成对象写入/读取/删除通过；测试桶其他前缀、正式桶读取及正式模型调用均返回 403，三个测试端点返回 200。测试 API 已接通，公网鉴权与环境隔离检查通过；手机实际认证仍未验收。

官方价目表核对后，健康测试部署强制使用每百万输入 12 元、输出 30 元的保守成本上界，覆盖 mini 音频最高档与 Pro 2.1 输出，月预算仍为 10 元；生产成本配置不变。部署校验 19 项通过，拒绝缺失和旧低价配置。见 `evidence/health-development-cloud-resources-2026-10-16.json` 和 `evidence/health-development-iam-isolation-2026-10-16.json`。

相册独立测试服务已通过登记的部署入口升级至 `42165fed`。保留匹配的 `b6bb8ba` 镜像和配置；20 项部署/恢复检查及部署后 API、worker 共六项环境/数据库拒绝检查通过。升级前恢复备份已认证；数据库、Redis、66 项 schema 及健康生产进程未改变，清理成功。真实 Apple 登录、媒体旅程及手机验收仍待完成。见 `evidence/albums-development-upgrade-2026-10-16.json`。

## 健康测试 API 接通（2026-10-16）

`761dc7b` 已部署，健康测试域名的 DNS/TLS、就绪 200、未授权配额及开发权益 401、内部路径 404、错误 Host 421 均通过。gateway/worker 正常运行，MySQL/Redis 容器与 66 项 schema 未改，健康生产进程未重启。启动时发现的手记 Prompt 依赖已修复；第一次发布自动恢复到 `7c0e6d2`，未对数据库做快照覆盖。修复后 58 个 Go 包检查通过，独立临时 MySQL 三项检查通过，真实测试库仅初始化八个 Health Prompt。部署清理已完成，上一版匹配镜像与配置保留。

mini 文本/音频、lite 文本和 Pro 图片的真实 Responses 完整响应、流式响应及 schema 校验均通过；不能替代识别质量验收。运维目录的测试恢复配置读取器已同步，异地备份结果见 `evidence/health-development-runtime-2026-10-16.json`。当前允许健康构建 4，对应新版 IPA 已交付；真实 App Attest、系统媒体上传、CloudKit 同步、正式升级及用户验收保持待办。

## 健康自动认证开发包交付（2026-10-16）

健康 Debug/Ad Hoc 1.0.2 (4)、源码 `100514f0` 已交付，包含自动开发认证和 Release 引导测试入口隔离。Xcode 27.1 的本机完整 unit 流程 1,624 项通过、零失败/跳过，iOS/watchOS Release 编译通过；七个签名目标及独立开发权限已核对。公网安装页、Manifest、完整约 149 MB IPA 的 SHA-256、HEAD/Range、等价 Host 和隐私响应头通过。安装临时上传目录已清理，签名包与证据保留于私有候选目录；正式健康进程未重启。此记录不代表手机安装、真实 App Attest、CloudKit、GitHub workflow 或完整 Release 门禁通过。见 `evidence/health-automatic-access-ota-2026-10-16.json`。

## 手记测试推理身份（2026-10-16）

`journalapp-development-backend`（97256858）已创建，仅绑定 `JournalDevelopmentInferenceOnly`，只允许三个新测试端点的 `ark:SendInference`；无控制台登录、Access Key、自管理权限。IAM API Key 已保存在获准的本机/服务器私有目录，0600/0700 与内容一致性通过。Lite、Pro、2K 插图真实调用通过，健康测试与手记正式端点均返回 403。创建当时 Pro 仍在切换版本，未立即替换服务；现已按下方“手记测试推理切换验收”完成切换。见 `evidence/journal-development-iam-2026-10-16.json`，服务切换已按后续记录完成，完整 App 旅程仍待验收。

## 手记独立测试语音（2026-10-16）

`journal-development`（手记开发测试）项目已开通流式语音识别 2.0 与录音文件识别 2.0，采用按时长后付费，未购买资源包；项目专用 API Key 保存在获准的本机/服务器私有目录，权限及内容一致性通过。约 35 秒的双人合成音频通过后端现有实时识别和录音文件识别测试，均返回六个语句、两位说话人；无需修改识别代码。首次文件任务出现一次“无有效人声”，同一音频的对照请求和原样 Go 复测随后成功，原因未确定，保留失败记录。

测试服务已通过既有部署入口切换到项目专用语音凭据，源码 `6d5a8a3`，旧语音备用凭据已移除；运行配置与进程一致，模型、开发连接和生产发布状态未变。原服务及匹配配置保留，部署前后加密备份均完成 COS 三对象读回、认证解密和手记隔离恢复检查。该次语音部署未替换推理凭据，后续已完成独立推理切换；手机录音与完整 App 旅程仍待验收。见 `evidence/journal-development-speech-2026-10-16.json`。

## 手记开发包 1.0 (20)（2026-10-16）

源码 `9a251b22` 已完成 Debug 归档、Ad Hoc 导出和两个目标的原生签名检查，独立 App Group/CloudKit、开发显示名称、测试 API 及三台登记设备均核对；IPA 不包含私有连接凭据。复用的 Debug/Release 环境边界测试与当前源码摘要一致，未宣称执行了新的完整测试或远端 CI。约 71 MB IPA 已通过私有 OTA 发布，公网完整下载摘要、Manifest、HEAD/Range、Host 和隐私响应头检查通过，上传临时文件已清理。签名产物私有保留，手记仓库未推送，手机安装/CloudKit/业务旅程仍待验收。见 `evidence/journal-runtime-boundary-ota-2026-10-16.json`。

## 手记测试推理切换验收（2026-10-16）

Pro 接入点已完成 260628 切换，控制台与真实响应一致。测试服务已通过既有部署入口激活 `ae02ac4`，Lite、Pro/语音改写、插图与独立 Prompt 修订一起切换到新测试端点，推理/图片均使用受限 IAM Key。语音项目凭据和开发连接未变，运行配置与进程逐项一致；旧服务和对应配置保留，清理通过，正式网关未重启。

公网鉴权、真实标签整理、插图生成与下载、五种文风的 WebSocket 改写通过；生成图片为 2496×1664，人工查看内容符合合成输入。测试 Key 调用正式模型返回 403。首次文风检查使用旧转录协议，没有触发待处理任务；已改为复用现有叙事请求与确认协议，保留事实、顺序和上下文边界断言，五项复测通过，未改业务逻辑。部署前后备份均完成 COS 三对象读回、认证解密和隔离恢复。

详见 `evidence/journal-development-inference-activation-2026-10-16.json`。本轮不改变 App 构建输入，不重跑 Xcode 或完整发布门禁；手机录音/应用保存、CloudKit 同步、真实权益和新定时备份周期仍待验收。

## 生日正式迁移准备（2026-10-16）

正式模型接入点 `ep-20261016124914-tttsd` 已创建，Pro 2.1 / 260628，6 RPM、60,000 TPM；测试身份调用该端点返回 403。专用 IAM 策略已保存于 `config/iam/birthday-production-inference-only.json`，身份/策略绑定/API Key 尚待已发出的授权确认，未提交创建。

发布就绪检查和定时巡检现在共用服务列表：启用生日时检查其配置的全部 Host，任一失败阻止就绪，巡检聚合为一项生日服务结果；后台接收与界面同步支持该结果。182 项运维测试中 181 项通过，1 项本机 Caddy 条件跳过；两个相关 Go 包及完整管理后台浏览器验收通过。首次新增浏览器断言未展开详情导致失败，展开后复测通过。此改动未部署，不能作为正式服务迁移完成依据；仍须完成身份、生产发布与路由、数据保留和真机业务验收。证据：`evidence/birthday-production-readiness-2026-10-16.json`。
