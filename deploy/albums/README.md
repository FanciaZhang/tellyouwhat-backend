# 相册独立测试环境

`cmd/albumgateway` 装配 MySQL 账户、加密 Apple refresh grant、单次登录挑战、可撤销会话、Redis 限流、上传/分片/图库/日常副本和订阅接口。`cmd/albumworker` 执行归档验证和媒体处理。新登录账户容量为零；必须启用并验证相册订阅才允许新增云备份。

`compose.albums-test.yaml` 是独立 Compose 项目：固定 `albums_test` 数据库、专用 MySQL 卷、专用 Redis 和网络、仅 `127.0.0.1:18090` 的 API 入口。迁移全部在这个新库运行。启动服务前检查身份表、上传/分片/派生/实况表与 COS 版本控制；运行中 `/readyz` 检查数据库、Redis 和退出状态。健康检查不等于 worker、Apple、转码或原件恢复通过。

当前 `tellyouwhat-albums-1251995834` 专用于相册开发联调；测试原件/派生前缀全部处于这个测试桶内。正式服务启用前配置独立正式桶与身份，正式数据不得接入此测试栈；不能通过复用这个数据库或桶把测试环境直接当作正式服务。

## 凭据与准备

1. 服务器 `/opt/tellyouwhat/albums-test/` 专用目录，所有者为部署用户，权限 `0700`。核对现有服务内存/磁盘余量、18090 端口和 Docker 子网，默认子网 `172.29.90.0/24`；实际 host gateway 必须与 `ALBUM_TEST_PROXY_CIDR` 一致。
2. `test.env.example` 复制为私有 `.env.compose-albums-test`，只供 Compose 插值；`runtime.env.example` 复制为独立 `.env.albums-test`，只供应用 `env_file`。两个文件均 `0600`；前者的 root 数据库密码不能传给 API/worker。密码分别生成，数据库/Redis 密码使用随机十六进制，避免 DSN/URL/Compose 转义问题。
3. Apple Sign in `.p8` 必须实际授权团队 `T6DD96HY2B` 与 App ID `cn.tellyouwhat.albums`。加密密钥、登录 IP HMAC 密钥各生成独立随机 32 字节，再以标准 Base64 保存到对应文件。应用拒绝同一密钥、非法格式、公共可读或可写文件。须保留加密密钥，换掉它会导致既有 Apple grant 无法解密。
4. `secrets/` 在宿主机设为 `root:65532 0750`，三个文件设为 `root:65532 0440`，让容器 UID/GID 65532 可读。只把该目录挂给 API；worker 不接收 Apple 私钥和账户加密文件。密钥不进入 Git、App、日志或聊天。
5. 腾讯 CAM 新身份分别命名 `albums-test-upload`、`albums-test-backend`，分别绑定 `cam-upload.policy.json`、`cam-backend.policy.json`；保留独立 Secret ID/Key。2026-10-12 经用户当次确认，两份策略已分别绑定；控制台核对各身份只有这一条直接策略、无随组权限。新密钥保存在本机与测试服务器的受限目录，未进入仓库。上传身份只写暂存；后端直接读写权限限于开发桶的相册范围，管理分片并查询/提交媒体任务。两份策略没有对象永久删除、ACL/桶管理、其他桶或 CAM 管理权限。若身份另有策略，必须核对总权限。

数据万象[异步任务接口](https://cloud.tencent.com/document/product/460/84789)还要求 `cam:PassRole`。`cam-media-role.policy.json` 只允许后端传递 `CI_QCSRole`；它不限制该角色本身可访问的 COS 数据，因此不能单独绑定来解锁处理。

2026-10-13 控制台只列出相册这一个 CI 桶，但共享 `CI_QCSRole` 自 2018 年存在，调整前关联 `QcloudPartAccessForCIRole`、`QcloudAccessForCIRole` 和 `QcloudCOSDataFullControl`，没有权限边界。桶列表不能证明所有旧业务都不依赖此角色。公开提交任务说明及当前 SDK `CreateMediaJobsOptions` 未提供可确认的自定义角色参数；不要编造一个独立角色并宣称任务会使用它。

**用户明确确认后已应用**：共享角色只绑定 `AlbumsTestCIMediaService`（289600252，版本 2），原三条策略已解除；`AlbumsTestCIMediaPassRole`（289601206）只关联 `albums-test-backend`。新服务策略只允许读取相册测试桶的 `albums/originals/*` 和 `albums/video-previews/*`，写入及分片管理仅限 `albums/video-previews/*`。当前视频及实况动态派生由 `SubmitVideoOnce` 使用此输出目录；静态图片仍通过现有同步图片适配器处理。没有对象删除、原件写入、桶管理、跨桶、CAM 或其他云服务权限。此替换限制整个共享 CI 角色，旧业务若依赖它可能停止处理；不能仅增加受限策略却保留原来的宽泛策略。

执行顺序：记录原关联策略 ID，创建并绑定 `AlbumsTestCIMediaService`，移除原三条策略并核对只剩新策略，然后创建/绑定 `AlbumsTestCIMediaPassRole` 到后端身份。以合成短视频验证当前桶内读取与派生输出，并验证越出目录/跨桶/写原件的任务失败，核对精确对象版本及大小；错误按最小所需权限定位，不能扩大为 `cos:*` 或 `cam:*`。自动处理仍保持关闭直到真实媒体链路通过。若需回滚，先撤销新增 PassRole，再恢复之前的角色关联，避免后端重新获得宽泛间接访问。角色会话与权限变更可能有传播延迟，必须重新验证，不能把控制台绑定成功当作隔离已通过。

首次真实任务在写 COS 时返回 `ResourceAccessDenied`，已先撤销 PassRole、再恢复原角色三条策略。对照[腾讯持久化权限说明](https://cloud.tencent.com/document/product/460/36239)，在服务策略及后端策略中补齐仅限预览目录的 `cos:UploadPart` 后重新应用；后端策略 289454608 当前为版本 2。第二次真实任务成功，输出 H.264/AAC 可完整解码；读取 staging、写入 originals、写入 derivatives 三项任务均以 `ResourceAccessDenied` 失败，源对象最新版本和字节未变。探针视频 89,230 字节、输出 93,014 字节，本次验证权限和处理连通，不证明压缩收益。跨桶、真实实况、完整应用及并发/大文件仍未验证，自动处理保持关闭。详见 `docs/evidence/albums-ci-role-change-2026-10-13.json`。

`python3 deploy/albums/cam_policy_test.py` 的 3 项检查只验证本地简单 allow-list 范围及 PassRole 目标，不模拟腾讯 CAM。真实探针通过 `ALBUM_CI_ROLE_PROBE=1 go run deploy/albums/ci_role_probe.go <合成小MP4> <输出目录>` 手动执行；固定测试桶和队列、每次随机键、不删除数据，不纳入自动测试。它仅使用后端测试身份，结果代表该身份与当前服务角色的组合权限。

正式视频适配器另用显式付费测试验证：`ALBUM_CI_ADAPTER_PROBE=1 ALBUM_CI_ADAPTER_DIRECTORY=<合成素材目录> go test -tags albums_live ./internal/albums -run '^TestLiveCOSVideoAdapter$' -count=1 -v`。目录内必须有不超过 16 MiB 的合成 `source.mp4`；结果为固定版本下载的 `output.mp4` 和不含凭据的 `adapter-evidence.json`。测试只用固定开发桶、随机原件/输出键，读取已授权的本机两份凭据，不删除云对象，也不启用 worker。2026-10-13 已覆盖 29.97 fps 无声、60 fps 无声及 90° 旋转的 30 fps 双声道素材，正式 Inspect/Submit/Poll/Find/Verify 全部通过，详细范围与首次失败见 `docs/evidence/albums-ci-video-adapter-2026-10-13.json`。这不替代 App 登录、上传、系统写回和原件删除的端到端验收。

权限格式及操作依据：[COS 分片上传](https://cloud.tencent.com/document/product/436/65644)、[COS 复制](https://intl.cloud.tencent.com/zh/document/product/436/78078)、[CI 授权粒度](https://cloud.tencent.com/document/product/460/41741)。CAM 文件通过静态范围检查也不证明真实服务会允许/拒绝；2026-10-12 已用合成字节验证真实暂存上传、普通 COPY 的固定版本归档/读取及哈希、分片初始化/上传/列举/合并；上传读暂存/写归档、后端越出相册前缀写入/读取桶 ACL 均返回 403。跨桶与分片复制仍待单独验证；永久删除拒绝及 CI 同桶目录边界的后续实测见本文对应记录。派生回收保持关闭，所需版本列举/永久删除权限后续单独配置和验收。

## 构建与接入

### 相册订阅与容量

`ALBUM_SUBSCRIPTIONS_ENABLED` 默认关闭。打开前配置相册专属 App Store API issuer、key ID、P8 私有文件、Apple 官方信任根 PEM；生产还要求相册数字 App Apple ID。私有文件与已有登录密钥一起挂载到 API 的 `/album-secrets`，不传给派生 Worker。测试部署只认 Sandbox，生产只认 Production，不使用其他 App 的权益或环境回退。

年度 SKU 草案为 `albums.cloud.50gb.annual`、`albums.cloud.500gb.annual`，容量分别为十进制 50/500 GB；这些标识尚未代表 App Store 中已经开售的商品，价格由 StoreKit 返回。购买时 `appAccountToken` 必须为相册稳定账户 UUID。`POST /v1/albums/subscription/transactions` 只接收交易 JWS；后端验证签名、App、环境和账户，再查询 Apple 当前全部订阅状态，不信任客户端声明的 SKU、容量或到期日。原始订阅链只能属于一个账户；多个有效链取最大容量，不叠加。账户修订号和签名时间拒绝过期并发结果。

`GET /v1/albums/subscription` 返回有效容量、已用与预留字节、有效期、核验有效期及六个自然月的保留截止日。没有已验证订阅的新账户无法新增上传，即使数据库旧投影残留容量也不放行。时钟到期或降档限制新增预留及 PUT/分片/实况上传授权；已预留且已上传的数据仍能完成校验，已用容量与原件保留。新授权链接同时受套餐有效期及 15 分钟核验期限限制；已签发链接在到期前仍可能有效，不能宣称退款瞬间撤销。

启用订阅时，API 同时接收 `POST /v1/albums/subscription/notifications`，无需用户会话，只接受 Apple V2 的 `signedPayload`，且严格验证签名、相册 App、环境和账户。配置通知 URL 时分别使用对应 Sandbox/Production 服务的 HTTPS 地址，不能混用。通知触发 Apple 当前状态查询；容量和语义指纹同一事务提交后才返回 200。相同 UUID 和内容的重复通知只确认已处理；签名无效返回 422，Apple/数据库故障返回 503，供 Apple 重试。TEST 仅记录已验证诊断事件，不授予容量。通知原文、交易 JWS 和私钥不入日志或通知表。

API 内四个工作协程核验持久订阅链：有效订阅每 5 分钟，失效订阅每天。数据库一分钟租约和令牌隔离多实例及重启，账户修订号拒绝迟到查询；失败按 1/2/4/8/16/32 分钟重试，保留已有状态。超过 15 分钟仍未成功核验时，`verificationPending=true`，保留现有内容与容量展示，暂时停止新增上传和签发写入授权。App Store 私钥只在 API，媒体 Worker 不持有。

到期提醒已具备默认关闭的发送调度，见下节；六个月后所有历史版本的永久清理仍需实现和验收，自动清理继续关闭。真实 Sandbox 购买、升降档、退款、通知与账号恢复也需要单独验收，受控 Apple 响应及测试证书的本地测试不证明真实商品已经可购买或 Apple 已接入此通知地址。

### 保留截止推送

迁移必须包含 0033。`ALBUM_PUSH_ENVIRONMENT` 控制设备注册，`ALBUM_RETENTION_PUSH_ENABLED=false` 独立关闭发送；打开前要求订阅核验启用、推送环境与 App 的实际 aps-environment 一致，以及独立 `ALBUM_APNS_KEY_ID` / `ALBUM_APNS_KEY_FILE`。P8 文件只放 API 的私有 `/album-secrets` 挂载，不复用登录或 App Store key，不进入 App/worker。完成真机验收前保持关闭。

每次领取都重新查询所有已知购买链，再逐设备复核账户修订和注册资格。新发现的账户所属购买链需完成权益对账后才能发送旧提醒。单次最多 16 个设备，未结束部分通过租约和逐设备回执续做。APNs 接收不代表设备显示，网络结果不明时可能重复，已经接收的通知无法撤回。

排查使用 `album_retention_deliveries.state`：accepted 表示 APNs 接收，pending 按 available_at 重试，rejected 表示当前注册代次的本阶段尝试已停止。父任务 delivered 仅表示阶段结束且至少一台设备被 APNs 接收。运行日志只记录通用延后事件，禁止记录设备令牌、密钥或完整 provider URL。没有可用设备时保留重查机会；自然登录会话到期不撤销已授权提醒，显式退出与已记录的授权撤销会阻止后续目标读取。

### 原件授权分享

`ALBUM_SHARING_ENABLED` 默认关闭。启用需要订阅环境明确为 Sandbox/Production、已核验的完整原件、`ALBUM_SHARE_SITE_ORIGIN` 的精确 HTTPS 网页源，以及独立 `ALBUM_SHARE_KEY_FILE`。该文件包含随机 32 字节的标准 Base64，与账户加密和限流密钥分离，权限同其他私有文件；只挂载到 API。轮换时把旧钥留在 `ALBUM_SHARE_PREVIOUS_KEY_FILE`，直至旧钥产生的有效链接及重试窗口结束（最长 30 天，领取授权最长一小时）。已派发的凭据使用数据库摘要验证，但缺少旧钥时不能重现旧链接或重试响应，不应提前移除。

已登录用户可为单个不可变版本创建、列举与撤销链接，有效期 1 小时至 30 天、领取上限 1–100，每份版本最多 32 个有效分享。提取码可选，4–64 个 Unicode 字符；仅保存 Argon2id 加盐证明。网页片段携带链接凭据，三个精确 POST 路径接收 JSON，不以查询参数传凭据。CORS 只允许该网页源，不使用账户 cookies 或浏览器 Authorization。

一次“领取”获取一小时下载授权，包含实况照片、配对视频及这份版本的其他原件；同一请求重试不额外计数。下载地址最长五分钟，固定 COS 对象版本，并受领取、链接及当前六个自然月保留截止日共同约束。刷新地址不再次领取；撤销停止新领取和地址刷新，已发出地址到期前可能仍可用。暂停或删除中的账户拒绝分享，不改变原件、系统副本和已用容量。原件可能包含位置等原始元数据。

图片与视频下载由 COS 直连，领取次数不等于实际下载次数。静态网页仅显示、领取和按需获取地址，没有上传或自动下载。真实 HTTPS/COS 下载、提取码、计数及撤销需独立联调后启用；本地受控服务测试不证明公网服务已经开放。

使用已评审提交的干净源码 checkout，将完整 Git SHA 写入 `ALBUM_TEST_IMAGE_TAG`。PR 容器构建只验证、不发布镜像；在独立测试目录按这个提交构建三份镜像，不能把移动的 `main` 标签当成相册 PR 的产物。

```sh
docker compose --env-file /opt/tellyouwhat/albums-test/.env.compose-albums-test \
  -f compose.albums-test.yaml config --quiet
docker compose --env-file /opt/tellyouwhat/albums-test/.env.compose-albums-test \
  -f compose.albums-test.yaml build albumgateway albumworker migrate
docker compose --env-file /opt/tellyouwhat/albums-test/.env.compose-albums-test \
  -f compose.albums-test.yaml up -d
curl --fail --silent --show-error -H 'Host: api.albums.tellyouwhat.cn' \
  http://127.0.0.1:18090/readyz
```

仅渲染 `Caddyfile.external` 的公开域名和端口并导入现有 Caddyfile；完整配置验证后 reload。核对相册公网 HTTPS、未知路径 404、其他既有域名健康。凭据环境文件不得交给 Caddy。测试失败可单独 `docker compose … stop albumgateway albumworker`；保留测试数据库和对象用于恢复，普通停止不用 `down -v`。

## 接入通过条件

- Apple App ID/Sign in Key 与真实授权匹配，退出、重启及另一进程的会话撤销正确；伪造转发头不能绕过限流。
- 以程序生成媒体完成真实原件上传、精确版本归档校验、图片/视频处理、完整实况对上传/读取/系统保存；记录源码 SHA、对象版本、大小/哈希、任务和计费证据。处理开关按实测结果逐项启用。
- 验证上传身份无法读取或写归档、操作其他桶；后端直接 COS 请求无法操作其他桶、修改权限或永久删除。另行核实 CI 服务角色的间接读写边界，不能用直接 COS 的拒绝结果替代它。不得用扩大到 `*` 权限解决 403。
- 为真实测试账户装配受审计的测试权益，或完成订阅权益验证后才授权容量。当前没有开放临时容量接口，也没有客户端指定账户/额度入口。
- 原件安全释放、实况/视频画质、真机发热/后台/微信/短信、费用和清理均独立验收。当前仅有受控服务装配验证，尚未实际部署此栈或启动付费处理。


回收站需要迁移 0034。`ALBUM_TRASH_ENABLED` 默认 false，只有明确值 true 开放 trash/restore；变更流和状态读取可提前接入更新后的客户端。启用前需验收原生删除标记、恢复及旧缓存升级；旧版本发表游标不具备跨设备删除同步能力。当前没有物理过期清理 worker，不得宣传已具备到期自动删除。

原件写入日志使用迁移 0038。升级前须停止并排空全部旧版相册验证 worker，再迁移并启动新版；不能在新旧归档 worker 混跑时执行，也不能回退到不检查写入日志的旧二进制继续处理队列。迁移把旧 `verifying` 与 `originals_verified` 资源标记为已消耗许可、旧协议记录：前者只核对可能已完成的 COPY，后者仍可读取已有备份。旧记录不能作为“只发过一次写入”的证据，必须独立核对遗留写入后才能放开永久清理。新进程启动会检查此表，未迁移时拒绝启动。

迁移 0039 为分片初始化补充持久核对次数、租约和退避时间，现有核对 worker 同时处理初始化与合并结果未知的任务。两类分别最多自动核对 3 次，共用批次数量上限；至少等待 2 分钟再读取，每次领取先消耗次数，崩溃后等待租约与退避。旧未决初始化保留原意图 ID 并进入核对，不重新初始化。日志 `initialized` 表示找回供应商会话，`assembled` 表示合并文件经过字节核验，均不代表备份发布或清理完成。过期任务只记录观察，不延长上传期限；次数耗尽仍保留未知状态与预留额度，不能通过重置次数或数据库超时自动宣称不存在对象。API/worker 在旧表结构上拒绝启动。

2026-10-12 实测修正：普通 COPY 的目标版本号位于响应 XML `CopyObjectResult.VersionId`，不能从 `x-cos-version-id` 响应头读取。共享复制方法已修复原件和实况路径，缺失或 null 版本仍拒绝确认。合成文件跑通现有 `AuthorizePut → PrepareSeal → Seal → OpenVersion`，但不包含完整账户 API、媒体处理、实际照片格式或 iOS 安全释放。见 `docs/evidence/albums-cos-cam-2026-10-12.json`。


Apple 账户通知需要迁移 0040。升级前停止并排空全部旧版相册 API，再迁移并启动新版；旧 API 不检查身份撤销截止，不能混跑或回退后继续提供登录。新 API 启动检查撤销表和授权签发时间列。既有授权的 NULL 签发时间按旧授权保守处理。

待相册 HTTPS API 可用并验证请求边界后，在相册主 App ID 配置服务器通知地址 `https://api.albums.tellyouwhat.cn/v1/albums/auth/apple/notifications`。此公开入口使用 Apple 签名验证，不接受 App bearer 作为真实性证据。只有提交撤销状态后返回 200，依赖不可用返回 503 供重试；重复事件安全。通知撤销账户会话与后续推送查询，不撤回已经签发的下载地址或在途推送；`account-deleted` 当前停用访问，完整物理删除仍须独立实现。当前尚未登记通知地址或收到真实事件。

2026-10-13 删除权限实测：`ALBUM_COS_PERMISSION_PROBE=1 go run deploy/albums/cos_delete_denial_probe.go` 只在固定测试桶的原件前缀创建一个随机名称的 48 字节合成对象。后端删除指定版本、上传身份删除同一版本、后端无版本删除三项均返回 403 AccessDenied；随后精确版本读取与字节校验通过。对象保留，不接触既有媒体。此结果覆盖这两个身份对本次对象的直接 COS 删除请求，不覆盖其他前缀、跨桶或 CI 角色间接权限。证据：`docs/evidence/albums-cos-delete-denial-2026-10-13.json`。
