# 相册独立测试环境

`cmd/albumgateway` 装配 MySQL 账户、加密 Apple refresh grant、单次登录挑战、可撤销会话、Redis 限流、上传/分片/图库/日常副本和订阅接口。`cmd/albumworker` 执行归档验证和媒体处理。新登录账户容量为零；必须启用并验证相册订阅才允许新增云备份。

`compose.albums-test.yaml` 是独立 Compose 项目：固定 `albums_test` 数据库、专用 MySQL 卷、专用 Redis 和网络、仅 `127.0.0.1:18090` 的 API 入口。迁移全部在这个新库运行。启动服务前检查身份表、上传/分片/派生/实况表与 COS 版本控制；运行中 `/readyz` 检查数据库、Redis 和退出状态。健康检查不等于 worker、Apple、转码或原件恢复通过。

当前 `tellyouwhat-albums-1251995834` 专用于相册开发联调；测试原件/派生前缀全部处于这个测试桶内。正式服务启用前配置独立正式桶与身份，正式数据不得接入此测试栈；不能通过复用这个数据库或桶把测试环境直接当作正式服务。

2026-10-14：测试域名已切到 `fb2cf96` 的 API/worker，Compose 项目 `tellyouwhat-albums-test-next`，本机端口 18091，独立数据库迁移至 0062。旧库备份完整恢复比对、迁移、新服务启动、公网鉴权及 Apple TEST 幂等重放通过。旧 API/worker 已停止，0040 数据库卷与配置保留；切换后产生的新数据不能通过回退旧卷丢弃。Sandbox 订阅核验开启，真实登录和购买未验收；媒体处理、分享、回收及正式销售保持关闭。[升级证据](../../docs/evidence/albums-test-upgrade-2026-10-14.json)。

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

照片适配器的真实付费探针独立于普通测试：显式设置 `ALBUM_CI_PHOTO_PROBE=1` 和 `ALBUM_CI_PHOTO_DIRECTORY=<新建证据目录>`，运行 `go test -tags albums_live ./internal/albums -run '^TestLiveCOSPhotoAdapter$' -count=1 -v`。仅使用批准的本机测试凭据与固定开发桶，生成三类合成图并保存六个预览，不读取用户图库、不删除对象；写入结果未知时不自动重复 PUT。重复运行会增加对象和处理费用。处理响应可缺版本头，但固定版本请求及前后原件 HEAD 核对不可省略；最终验收记录见 `docs/evidence/albums-ci-photo-adapter-2026-10-13.json`。

实况真实探针位于 `internal/albums/cos_live_roundtrip_test.go`，同样仅在 `albums_live` 与 `ALBUM_CI_LIVE_PROBE=1` 下执行。`ALBUM_CI_LIVE_DIRECTORY` 指向 Xcode 管理运行目录内的合成素材目录：先用 App 的 `testPrepareRealCILiveSource` 生成配对原件，再单独运行 `TestLiveCOSLiveComponents`，随后由 `testAssembleRealCIComponents` 重建配对，最后单独运行 `TestLiveCOSLivePair` 并由 `testRealCanonicalCIPairSystemSaveAndRecovery` 验证系统保存。每次 Go 测试使用精确 `^名称$`，不要一次运行所有阶段；原生阶段显式设置 `ALBUMS_PHOTOKIT_FIXTURES=1` 和 `ALBUMS_CI_LIVE_PATH`。

结果未知时不重跑写入阶段。已记录任务可通过 `TestLiveCOSLiveResume` 只读核验，成品下载签名过期可通过 `TestLiveCOSLiveRefreshReadGrants` 重新读回并签名；两者不新建媒体任务或对象。目录中的 `read-grants.json` 含短期授权 URL，保持私有且不复制到仓库证据。独立 `version.json` 是 App 测试所需的受控目录夹具，绑定本次资源 hash、timing 与 versionID，不代表服务器已发布完整备份。测试不删除云端对象，也不授权原件释放。


2026-10-13 App Store Connect 已创建相册 App `6822133304`，环境示例已填写该数字 ID。订阅群组 `22470252`，`albums.cloud.500gb.annual`（Apple ID `6822133711`，等级 1）和 `albums.cloud.50gb.annual`（`6822134191`，等级 2）均为未提交草案。尚无真实 StoreKit 购买、通知或权益核验验收，App Store Server API 凭据与商品可用性仍待配置。此次仅更新示例，不修改运行环境或开启订阅。


### 原件低频存储（2026-10-13）

开发桶已通过控制台保存 `originals-lifecycle.xml` 所描述的唯一规则：`albums/originals/` 当前版本 30 天、非当前版本 30 天后转 `MAZ_STANDARD_IA`，无删除动作。预览和暂存不匹配此规则。该 XML 是可审查的规则记录；不要直接覆盖将来新增的其他规则，COS PUT lifecycle 会替换完整规则集。现有业务身份没有桶管理权限，无需为了该规则扩权。

生产写入仍走标准存储；生命周期异步转换。小于 64 KB 的对象不会经生命周期转低频，低频最低存储计费 30 天。读取没有解冻步骤，但取回另收费。停用规则只阻止未来转换，已转换原件不会自动回标准；回转须另外评估费用及版本。此规则与订阅保留/永久清理完全分开，不允许加按对象年龄过期删除。

真实兼容性探针在已有照片/视频命令上加 `ALBUM_COS_PROBE_STORAGE_CLASS=MAZ_STANDARD_IA`。只改变新建合成原件的初始类型，并核验精确版本 HEAD、原件流读取、签名下载字节，以及输出 `MAZ_STANDARD`；未设置变量保持原探针行为。仅允许此值，不接受任意归档类型。每次新建对象并发生存储/处理费用，不删除素材，不以重跑处理未知结果。

2026-10-13 三类图片和有声视频通过，视频完整解码通过。证据：`docs/evidence/albums-low-frequency-2026-10-13.json`。这不证明真实生命周期已执行、iOS 全链路实况恢复或用户账单收益；后续按真实对象 HEAD 和账单核对。本次仅增加验证与桶配置，不要求重启/部署服务。


2026-10-13 订阅定价更新：中国大陆年付 50 GB 228 元、500 GB 1,998 元，均配置两周免费试用，未开放销售。现有服务端以 Apple 当前签名交易 expiresDate 和订阅状态发放权益，不要求正数价格，也不把年度 SKU 固定授权一年；新增零元两周有效/已过期交易回归，两项父测试（含子用例）通过。试用结束未付费仍适用六个自然月保留。真实 Sandbox 免费领取、取消、转付费与通知尚待验证；本次无服务端运行逻辑或数据库变更。

2026-10-13 Sandbox 实测更新：相册内购密钥 `Albums Test Backend` 已配置至隔离测试 API，启用订阅核验；现有 c8760dc 镜像及数据库结构未变。ASC 仅设置 Sandbox 通知 URL。Apple TEST 通知送达状态为 SUCCESS，服务端验签并入库；重复投递两次仍只有一条记录，账户和订阅数均为零，无效签名返回 422。此结果不证明真实购买、续订、退款或账户联调已通过；正式销售、照片/视频处理及永久回收继续关闭。证据见 `docs/evidence/albums-sandbox-notifications-2026-10-13.json`。

配置更新使用 `configure_sandbox.py prepare` 保存私有 before/after 配置和镜像/密钥引用，再通过 `deploy/tencent/ops_artifacts.py --apply --register --owner albums-test ... --run python3 .../configure_sandbox.py apply ...` 激活。该流程只重建相册 API，失败时恢复旧配置。共享运维检查为相册增加正确 Host 和 `{code:ok}` 检查，生日服务仍使用原协议。不要直接运行 apply 绕过注册和锁。

数据库跨版本升级先使用 `stage_test_upgrade.py --source <完整提交> --run albums-upgrade-<唯一名称>`，通过同一 `ops_artifacts.py --run` 包装运行。这个首次升级工具仅接受尚无账户和上传的独立测试栈；不用于已有用户的迁移。它短暂停止旧 API/worker，保存加密快照后恢复旧服务；在 `tellyouwhat-albums-test-next` 的独立 MySQL 卷、Redis、网络和 18091 本机端口恢复并逐字节核对数据库，再执行迁移和新版服务启动检查。原数据库卷、旧镜像、配置及密钥引用保留。失败停止候选 API/worker，保留数据和私有诊断，不删除卷或切换公网路由。成功只代表升级演练，不能宣称已部署公网；切换前必须重新冻结写入、同步最新快照并验证候选。备份密钥需单独保管，候选卷不替代离机灾备。包装器登记新旧镜像和 `.operations/<run>` 选择记录，健康检查仍指向旧栈 18090。验证：`python3 -m unittest discover -s deploy/albums -p 'stage_test_upgrade_test.py' -v`。

演练通过并在本机另存验证过的加密快照后，`promote_test_upgrade.py --source <演练镜像的完整提交> --run <同一演练>` 在包装器内执行切换，包装器检查地址改为 `http://127.0.0.1:18091/readyz`。它核对配置、密钥和候选镜像，停止旧写入后重新导出旧库；若与演练快照不一致则恢复旧服务并拒绝切换。数据未变时只修改 `/etc/caddy/albums-test.caddy` 的上游，验证配置、reload 后读取 Caddy 实际载入配置并检查公网。reload 开始前失败可恢复旧服务；reload 结果未知或之后失败时保留候选服务供向前修复，不自动回退数据库而丢失新写入。`promotion-started` 防止未知结果被重复执行。旧数据库卷和二进制仅作为旧快照恢复材料，不能在新服务已接受写入后直接切回。此工具仍不开放销售、媒体处理或物理删除。

本次独立探针：`go run deploy/albums/appstore_probe.go request`（只允许一次 POST），随后 `go run deploy/albums/appstore_probe.go status`；固定相册 Sandbox、密钥 ID 与本次私有证据目录，不用于生产。重复查询状态不会再请求通知；遇到未知 POST 结果应保留记录排查，不能删除标记重试。Apple token 是不透明字符串。私钥、请求 token 与通知 JWS 只留在本机受限目录，仓库仅保存验签结果及摘要。探针私有文件边界检查：`go test deploy/albums/appstore_probe.go deploy/albums/appstore_probe_test.go`。

### 账户注销清单权限（2026-10-13，待确认保存）

`cam-backend.policy.json` 已准备新增 `cos:GetBucketObjectVersions`，用 `string_like` / `cos:prefix` 限制到相册五类媒体前缀（暂存前缀包含实况暂存）。当前在线策略仍缺少此权限，真实探针返回 403；浏览器编辑表单未保存。批准保存后须验证固定合成账户的版本清单及范围外前缀被拒绝。不得把此草案当作已部署权限或永久删除授权。


### 注销媒体核对 worker（2026-10-13，未部署）

新版 `albumworker` 默认调度已受理注销账户的原件、照片、视频和实况观察任务；只读取对象 HEAD / CI 状态，不提交转码、上传或媒体删除。它不依赖照片/视频生成开关，也不需要 Apple 私钥。每类每轮使用现有 batch size 上限（最多 100），观察失败按各自记录退避；日志 acknowledged 指写入结果或任务终态核对，不是账户擦除完成。

升级须停止并排空旧相册 API/worker，迁移至 **0050** 后同时启动新版，不能与旧版处理进程混跑；旧版不维护实况复制回执、照片注销版本约束或普通 PUT 授权记录。启动检查迁移缺失即失败。保留数据库、对象及可恢复部署材料，回退需重新验证写入协议兼容性，不能仅切回旧镜像继续处理。此处为部署要求，线上仍以最近部署证据为准。


普通 PUT 清理保护需要迁移 0049。旧记录默认视为可能发出过普通 PUT；不能批量改成 FALSE 来解除清理限制。新 API 在返回地址前原子保存 TRUE；只使用受控分片的新原件记录保持 FALSE。旧 API 不得在迁移后继续签发地址，否则会漏记授权。实况普通 PUT 仍需迁移到可终止会话，相关永久清理继续关闭。

真实跨到期探针：显式设置 `ALBUM_CI_LIVE_PROBE=1`、`ALBUM_CI_LIVE_DIRECTORY=<新建私有目录>` 后运行 `go test -tags albums_live ./internal/albums -run '^TestLiveCOSPresignedUploadExpiryInFlight$' -count=1 -v`。每次新增一个约 29 KB 的合成暂存对象，探针不删除它；返回值仅表示观察过程成功，具体行为以 JSON 中 `completed_after_expiry` 为准。2026-10-13 的一次实测为 true，不能依赖签名到期停止在途 PUT。证据见 `docs/evidence/albums-direct-upload-expiry-2026-10-13.json`。


### 实况分片协议（2026-10-13，未部署）

迁移 0050 与新版 gateway 必须一起使用。已有 Multipart 配置启用时，新增 `/v1/albums/live-pair-uploads/{pairUploadID}/resources/{resourceID}/multipart` 初始化/查询、`/parts/{partNumber}/grant` 签名和 `/complete` 合并接口；只接受 photo/motion。普通 PUT 路径与分片路径互斥，不能把旧记录的 direct_upload_issued 改成 FALSE。缺失会话表时启动检查失败。

内部服务提供只读核对及注销后终止会话，但后台调度和 iOS 接入尚未完成，不能据此打开永久清理。真实终止探针 `TestLiveCOSLivePairMultipartAbortInFlight` 使用既有 `ALBUM_CI_LIVE_PROBE=1` 和全新私有 `ALBUM_CI_LIVE_DIRECTORY`，创建一份合成分片并 Abort 本次会话；不执行 Complete 或删除对象版本。同目录存在 intent 标记时拒绝再次初始化，未知结果须保留已写入的意图和会话记录。证据见 `docs/evidence/albums-live-pair-multipart-2026-10-13.json`。

### 账户媒体清理调度（默认关闭）

`ALBUM_ACCOUNT_MEDIA_ERASURE_ENABLED=false` 与派生临时清理独立；启用需要迁移 0057。调度只处理已受理注销且全部写入已收敛的账户，复用固定版本/分片计划，最后重新列举六个区域并结算媒体索引和额度。调度期限持久化，错误退避，崩溃可重领；已有对象操作租约仍约束每个删除动作。日志只有计数，没有账户、对象键或供应商错误内容。

当前测试环境没有对象永久删除权限，不应打开此开关。开放前仍须完成受限删除身份及实际权限验收、最终身份与订阅恢复规则、备份删除、HTTP/UI 和完整流程；媒体结算不表示账户删除完成。本轮隔离验证见 `docs/evidence/albums-account-media-erasure-worker-2026-10-13.json`，没有部署或真实删除。

### 订阅身份与媒体账户分离

迁移 0058 为 Apple 登录身份生成独立的 StoreKit UUIDv5，命名空间为 URL、名称为相册专用前缀和已验证身份的 SHA-256。数据库生成列不可由客户端赋值，并建立唯一索引。订阅响应提供 `appAccountToken`；原生新购买使用该字段，服务端交易核验、通知归属、后台刷新和到期前全部购买记录核对同时接受它。媒体账户 ID 仍为独立随机 UUID；相同 Apple 身份重新注册时能重算订阅标识，不复用媒体目录或缓存标识。

已有账户 UUID 绑定的旧交易仍仅属于其原账户，不接受客户端转移。旧式交易的 Apple 重新绑定已实现默认关闭的后台队列，真实改绑及最终身份/订阅/备份擦除尚待验收，完整账户注销不得开放。本阶段测试通过直接清除隔离数据库中的合成账户模拟“旧记录已不存在”的边界，不能替代注销执行器或真实 Sandbox 恢复购买验收。旧 App 仍可能使用旧字段；发布前必须完成客户端/服务端协同升级。

Apple 字段与绑定规则依据：[appAccountToken](https://developer.apple.com/documentation/appstoreserverapi/appaccounttoken)、[Set App Account Token](https://developer.apple.com/documentation/appstoreserverapi/set-app-account-token)。本轮未调用重新绑定接口，也没有部署或修改真实订阅。

### 注销前旧订阅改绑（默认关闭，未部署）

迁移 0059 与 `ALBUM_SUBSCRIPTION_REBINDING_ENABLED=false` 配套。只有启用订阅验证且显式打开此开关，gateway 才运行改绑队列。注销受理事务保存已知购买链；后续已验证通知补充新链，并使对应链的旧回执失效。任务校对注销请求、账户状态、服务端生成标识、环境及租约；先读取 Apple 当前签名状态，仍为旧绑定时才调用 PUT，之后再次读取签名交易确认。HTTP 200 本身不构成改绑证据。响应丢失时退避，下一轮先读取，已改绑则不重复 PUT。

Apple 的 [Set App Account Token](https://developer.apple.com/documentation/appstoreserverapi/set-app-account-token) 只更改当前交易及未来续订的标识，历史交易不会改写，家庭共享交易不支持。最终身份擦除还须核对所有相关购买链及灾备；这里的回执不能单独授权最终擦除。真实 Sandbox 的历史交易恢复行为仍需设备验收。相册 API 已使用文档中的 `api.storekit-sandbox.apple.com` / `api.storekit.apple.com` 域名，Health 配置不变；仅对现有 TEST 通知做了真实只读查询，没有发送真实改绑请求。

隔离测试覆盖租约、重启、撤销账户、签名与归属、后续通知、失败退避和事务回滚；证据见 `docs/evidence/albums-subscription-rebinding-2026-10-13.json`。升级前排空旧相册进程并迁移至 0059，不能混跑忽略队列维护的版本。没有部署或打开销售、永久清理及改绑开关。


改绑查询也会检查响应中同一相册账户的其他购买链：逐条验签并完整核对该链的状态与续订信息后，未知链及其待改绑任务在同一事务落库。存在他人归属、同链冲突、过期执行租约或写入失败时整批回滚，且不发起本次 Apple PUT；超过 128 条关联链时拒绝截断。已知链不因重复出现在响应中被反复入队，其后续变更仍由通知处理及最终全链核验负责。此发现机制只覆盖 Apple 当前查询实际返回的记录，不能宣称已经枚举从未同步的全部历史购买；最终账户擦除仍未开放。验证见 `docs/evidence/albums-subscription-discovery-2026-10-13.json`。


最后一条待改绑回执提交前，新版会重新查询该账户的每一条已知购买链，核对当前签名交易全部使用稳定标识。查询遗漏、仍有旧标识、异常签名或新发现购买链都会保留待处理状态；新链先原子入队。最终事务重新核对账户订阅 revision、购买链集合、交易锚点、签名时间及任务回执；一分钟内的核验结果才可提交。两个执行者同时完成不同任务时，后完成者也必须满足完整核验条件，不能凭此前的局部快照结束最后一项。此检查复用现有默认关闭的改绑队列，不增加迁移、权限或进程。

该检查证明改绑队列本次收尾所见的已知购买链一致，不构成最终身份/灾备擦除许可；之后收到通知仍会重新打开对应任务。完整账户注销仍须完成最终事务与备份策略，不能把旧回执长期作为删除依据。证据：`docs/evidence/albums-subscription-completion-2026-10-13.json`。

迁移 0062 在存储账户上保存全链核验的 `subscription_checked_revision` 与 `subscription_checked_at`。只有最后一个任务在有效租约内通过同一版本的完整复查，才与该任务回执一起提交。订阅版本更新时在同一行同时清除核验标记；数据库约束也拒绝保留与当前版本不符的标记。迁移不推断旧结果，会重新排队已确认的改绑任务以重新读取 Apple；已绑定的交易不因此重复发送改绑请求。升级前排空旧相册 API/worker，完成迁移后同时启用新版；不可混跑或回退至不认识这些字段的版本。启动检查拒绝缺失字段的数据库。

标记只是核验时刻的全部已知购买链证据，不能代替最终事务中的当前请求、版本、时效和其他清理条件，也没有证明发现全部从未同步的购买记录。它不表示身份或备份已经擦除，不启用任何删除入口。隔离事务、迁移与恢复验证见 `docs/evidence/albums-subscription-completion-receipt-2026-10-14.json`。


### 相册加密快照与隔离恢复演练（未部署）

通用 `deploy/tencent/ops_backup.py` 保持原控制数据白名单，不保存相册数据。新增 `deploy/albums/backup.py` 只导出经审核的 36 张 `album_*` 表，涵盖媒体索引、固定对象版本、账户关系、订阅、删除意图及未决写入。登录授权的加密字段、会话哈希等恢复所需状态也在加密快照内；明文运行配置、加密密钥和其他 App 表不进入快照。发现缺表、新增相册表或非 InnoDB 表时停止，不能静默丢失新业务数据。

使用单次 `mysqldump --single-transaction` 导出，复用现有 AES-256-CBC/PBKDF2 加密与 HMAC 清单认证，在解密前核对密文摘要、数据库、相册专用范围及生成时间。采用独立 `ALBUM_BACKUP_ENCRYPTION_KEY` 和 0700 目录，真实配置参照 `backup.env.example`，文件权限 0600；独立密钥和原有凭据加密密钥仍需另行异地托管。不会因共享服务切换部署版本而替换显式指定的相册数据库配置。共享控制备份拒绝导入相册范围快照。

命令接口：`backup.py backup|drill|prune --backend-dir <operations-root> --environment-file <private-albums-backup.env> --backup-dir <private-albums-vault> --mysql-bin-dir <MySQL-8.4-bin>`。`drill` 另需 `--backup <filename>`；只接受本工具的文件名，不接受线上恢复目标。共享主机执行须遵守仓库 `AGENTS.md` 的 ubuntu 所有者、受登记 producer 和操作锁要求，本文不表示已登记、部署或启用定时任务。

演练在新建私有临时目录启动仅有 Unix socket 的 MySQL，恢复后比较各表行数及按主键顺序重新导出的行内容 SHA-256，包括 JSON 和二进制字段；然后写入持久的数据库恢复隔离标记，并停用恢复出的活动账户、会话、分享和推送。删除中的账户保持 deleting。全过程不启动 API/worker、不更改源库，结束后停止临时 MySQL 并删除临时数据。演练解压上限 32 GiB，超过时失败，不截断。

本地快照按经过认证的生成时间保留 14 天，`backup` 前或显式 `prune` 清理到期快照，以及 `fetch` 生成的恢复目录（包含目录内的快照、清单和注销日志副本）。恢复目录沿用快照签名中的原生成时间，重新下载不能延长保留期；根目录独立注销日志及云端对象不在此清理范围。清理前核验完整文件清单与认证信息，签名清理回执支持中断后的剩余文件重试；未知文件、链接或内容变化会停止并保留现场。损坏或签名异常的文件保留并报错，mtime 不能延长保留期。此期限是工具的本地快照策略，不能替代账户注销或媒体到期删除传播。加密恢复包的异地传输已实现但未部署，删除日志的独立异地发布与最新性证明、删除账户的快照消除、恢复后的线上接管和凭据恢复验收；隔离演练成功不等于灾备就绪，更不能用来开启永久清理或账户注销。

验证见 `docs/evidence/albums-recovery-backup-2026-10-13.json`。


### 删除意图与恢复清单（迁移 0060，未部署）

`album_deletion_ledger` 与注销受理在同一事务写入，保存账户 ID、请求 ID、原受理凭据的哈希和 UTC 时间；不保存原始凭据、媒体或 Apple 身份。没有指向活动账户的外键，最终账户移除不能连带抹掉恢复所需的意图。迁移回填已有受理记录，启动缺表检查拒绝旧结构。所有旧相册 API 必须排空并协同升级，否则旧进程可能受理请求却不写日志。当前尚未执行最终账户移除。

`backup.py ledger` 使用相同私有配置导出单独的、HMAC 认证的清单，返回文件名、SHA-256 和数量；这是本机文件，明确记录 `offsite=false`。导出检查所有现存受理记录均有一致日志，遗漏或冲突就失败。采用全量读取，避免把事务提交先后误当成自增序号的连续进度；上限 100,000 条，超过时拒绝截断。文件包含不可公开的关联标识及凭据哈希，0600 保存，不能作为公开分享文件；没有自动清理日志或其历史检查点。

恢复演练可显式提供 `--deletion-ledger <private-file> --deletion-ledger-sha256 <expected-sha256>`。核对同一批文件字节的摘要、签名、数据库、范围、记录格式及快照时间顺序后，逐项比较快照内已有日志和受理记录；缺项、改写或冲突会中止。匹配记录在隔离恢复库中重建注销受理、账户状态和存储禁写时间，补齐 Apple/媒体任务及订阅改绑队列，并使旧改绑回执失效。之后再检查意图数量和禁写状态，全部恢复账户仍处于隔离停用状态。不会启动 API/worker 或执行真实删除。

指定摘要只能锁定这次演练的输入，不能证明它是云端最新清单。异地发布确认、故障切换时的最新检查点选择、快照生成后未知写入的核对及物理快照消除仍待实现；不提供清单的普通 `drill` 会明确输出 `deletion_ledger_applied=false`，不能据此进行线上接管。即便提供清单并演练成功，也不授权开放服务或永久清理。验证：`docs/evidence/albums-deletion-ledger-2026-10-13.json`。


### 恢复库启动隔离（迁移 0061，未部署）

仅停用账户不足以阻止旧媒体后台任务。恢复演练现在把 `album_recovery_state` 标记为隔离，API 和 worker 在 Redis、COS、Apple 客户端及后台调度启动前检查它；表或唯一状态行缺失也拒绝启动。媒体事务共用的存储账户锁及媒体权限检查使用当前共享锁读取，旧事务快照不能绕过隔离，不同账户不因该检查被全局串行化。新建数据库默认允许正常启动。

这是离线恢复保护，不能用作运行中服务的紧急停止开关，也不能撤回已经发出的云请求。尚无解除隔离或线上接管入口；完整切换必须另外核对最新删除清单和快照之后的在途写入。升级须先排空旧相册 API/worker，运行迁移，再协同启用新版；旧二进制不识别该标记，不能混跑或用于恢复库。验证记录：`docs/evidence/albums-recovery-quarantine-2026-10-13.json`。

### 加密恢复包的 COS 传输（未部署）

`backup.py publish` 把已认证快照、快照清单和显式指定的删除清单复制核验后打包，再整体加密。删除清单不会以明文上传。`cmd/albumbackup` 复用现有 COS SDK，只处理 `albums/recovery/<随机ID>.enc`：先检查版本控制已启用，上传后按返回版本完整读回并比较字节数与 SHA-256，成功才写本机 HMAC 签名发布回执，再上传到同一桶、同一随机 ID 的 `.receipt.json` 对象并固定版本回读核验。两个对象均确认后才返回发布成功。ETag、上传成功响应或 HEAD 均不能代替回读。拒绝 HTTP 重定向，未知 PUT 结果不自动重试；失败可能留下未确认对象，不能记为可用备份。

构建 `go build -o <受管理输出目录>/albumbackup ./cmd/albumbackup`。使用单独的私有版本化备份桶和受限身份；配置格式见 `backup-cos.example.json`，真实文件 0600，密钥不得进入仓库。`cam-backup.policy.template.json` 仅为待填写模板，要求查询版本控制和备份前缀的读写，不含媒体读取、删除或桶管理权限；尚未创建或绑定这份策略。备份与媒体身份分开，不能把备份文件放进用户原件目录来绕过权限。

在已有 `backup.py` 私有配置参数后使用：

```text
publish --backup <snapshot.enc> --deletion-ledger <private-ledger.json> --deletion-ledger-sha256 <sha256> --transfer-bin <albumbackup> --cos-config <private-backup-cos.json>
fetch --receipt <private-publication-receipt.json> --receipt-sha256 <sha256> --transfer-bin <albumbackup> --cos-config <private-backup-cos.json>
```

`fetch` 先认证固定摘要的回执，核对配置中的桶与地域，仅下载回执中的固定对象版本；再次验字节摘要后解密，只接受指定的三个普通文件，拒绝重复项、路径穿越和链接。不覆盖现有备份，输出到新的 0700 目录，各文件 0600。输出的快照、删除清单及摘要可用于现有 `drill`，恢复仍被隔离；不会启动服务。源快照超过 14 天时拒绝发布/下载。当前每个加密包最多 1 GiB，超限直接失败，扩大前须实现分片传输；不压缩或截断数据。

发布回执保存在本机及备份桶的 `albums/recovery/<同一随机ID>.receipt.json`，其中不含解密密钥或原始凭据。若回执上传失败，本机签名文件保留；使用 `publish-receipt --receipt <file> --receipt-sha256 <sha256>` 及同样的传输参数只重发回执，不重传备份包。失败或未知响应不宣称发布成功。灾后可从 COS 控制台取回该回执，计算文件摘要后交给 `fetch`；仍须通过独立密钥的 HMAC 认证。密钥须独立托管。回执锁定一次确认过的上传，不能证明故障发生时它仍存在，或内含最新注销清单。自动故障切换、最新检查点发现和按账户擦除仍未完成，也未验证真实 COS 权限与恢复往返。远端到期清理的实现与限制见下节。受控 COS HTTP 测试、真实 OpenSSL 加解密和隔离 MySQL 恢复证据见 `docs/evidence/albums-offsite-bundle-2026-10-13.json`。

### 云端恢复包到期清理（未部署）

上传前先持久化带 HMAC 的本地发布意图（同一 `albums-offsite-<ID>.json` 文件，`publication_confirmed=false`、版本号为空），记录目的桶/地域、精确键及加密包摘要和长度，不保存 COS 密钥。必须先落盘成功再发 PUT，传输请求也锁定该目的地。只有固定版本读回校验成功才原子更新为已确认回执；未确认的文件不能用于 `fetch` 或 `publish-receipt`，也不是备份发布成功的证据。旧的已确认回执仍兼容。

PUT 响应丢失或随后确认记录写入失败时，本地意图保留。源快照到期后，可将该文件及其当前摘要交给 `expire-offsite`；工具检查实际版本，没有上传成功也必须列举确认为空。这个保护依赖本地签名文件尚存，不能覆盖本机及意图文件同时丢失的灾难；真实异地意图保护及全桶孤立对象核对仍待完成。

`expire-offsite --receipt <private-publication-receipt.json> --receipt-sha256 <sha256> --transfer-bin <albumbackup> --cos-config <private-expiry-cos.json>` 复用 `backup.py` 的私有配置及操作锁，必须指定已认证且源快照到期 14 天的发布回执。未到期、错误数据库或被修改的回执不能进入传输工具。操作只处理回执对应的一个恢复包及同名云端回执，不扫描媒体，不自动清除本地回执或独立注销日志。

工具先完整分页列举这两个精确键的当前/历史版本和删除标记（最多 10 页、10000 个版本）；逐个固定版本下载，核对长度与 SHA-256 后才开始删除。陌生键、分页异常或内容差异会在删除前停止。删除始终携带小写 `versionId` 参数。末尾再次列举，只有完整结果为空才记录成功；未知删除响应、权限错误或清理期间出现新版本均报错，可使用同一签名回执重试。不同内容的历史版本保留并要求排查。验证下载会产生备份读取流量；当前 1 GiB 单包上限保持不变，单次所有版本核验总量超过 4 GiB 时在读取对象前停止，需另行评估处理成本。

权限草案为 `cam-test-backup-expiry.policy.json`：仅指定备份桶恢复前缀的列版本、读取和删除；无上传、媒体访问或桶管理权限。应使用独立清理身份，不能给现有上传身份追加该策略。文件仅为待审批配置，没有创建身份、绑定权限或部署定时清理。新命令需要显式调用，普通 `backup`、`prune` 和 `fetch` 不会触发云端删除。

依据：[COS 版本删除](https://intl.cloud.tencent.com/document/product/436/7743)、[列举版本](https://www.tencentcloud.com/document/product/436/31551)、[前缀及版本条件](https://cloud.tencent.com/document/product/436/71306)。清理回执只证明本次查询时该包的版本为空，不证明整个账户的所有历史数据已消除；丢失回执的孤立上传、其他包、独立删除日志和按账户擦除仍需处理。


备份资源核对（2026-10-13）：控制台显示现有 `tellyouwhat-backups-1251995834` 为广州、多 AZ，版本控制未开启，已有 `backups/` 目录。未修改该桶。已填写独立 `tellyouwhat-albums-backups-1251995834` 的私有、多 AZ、版本控制配置，但尚未提交；专用 `albums-test-backup` 身份及 `cam-test-backup.policy.json` 权限待用户确认后创建。此策略只含新桶的版本状态查询和 `albums/recovery/*` 的读写，不含删除、其他 App/媒体目录或桶管理。截图仅证明待提交配置，不能作为资源创建证据。签名回执的受控验证见 `docs/evidence/albums-offsite-receipt-2026-10-13.json`。


### 注销受理 HTTP 合约（保持关闭）

`POST /v1/albums/account/deletion` 只接受 `requestID` 和会话 Bearer；归属由数据库内的凭据决定。首次受理与撤销会话、关闭分享/推送及写入删除队列、独立删除日志同事务提交。`202` 只表示 accepted，不能显示“已删除完成”，也不取消 Apple 订阅或删除系统图库。客户端必须在发送前持久保存请求编号和原凭据；响应丢失后仅用完全相同的组合重试。普通认证中间件会拒绝已撤销会话，因此只有此 POST 交给事务内的首次/精确重放认证，其他路径照常认证。

Gateway 保持未注入处理器，没有新增环境开关；当前请求返回 503。最终身份、订阅记录与灾备擦除及原生确认/重试状态尚未完成，不能据此启用用户入口。无数据库迁移或部署。验证包含真实隔离 MySQL/Redis 的 42 个顶层测试、104 个通过事件及 Swift 合约 18 项，零失败/跳过；见 `docs/evidence/albums-account-deletion-http-2026-10-13.json`。


### 未受理注销的撤回（2026-10-14，保持关闭）

`POST /v1/albums/account/deletion/withdrawal` 使用原会话 Bearer 和同一 `requestID`。200 的 `state=withdrawn` 表示尚未受理且原凭据不能再受理迟到请求，客户端需重新登录；`state=accepted` 带原始 `requestedAt`，表示已经受理、不能撤回。不得只看 HTTP 200 判断撤回成功。客户端必须先持久保存撤回意图，未知响应后只重试撤回，不能再发送原注销 POST。

受理与撤回共享身份/账户锁；撤回仅吊销原会话和该会话推送，不关闭其他设备。原会话消失时核对独立删除日志，匹配已受理记录则仍返回受理回执，不把访问记录擦除误认为尚未受理。Gateway 两个入口继续关闭，无新增迁移、权限或部署。隔离数据库竞态/回滚和 19 项 Swift 合约通过，见 `docs/evidence/albums-deletion-withdrawal-backend-2026-10-14.json`。

原生两进程联调：相册仓库 `scripts/test-native-roundtrip.py` 启动隔离 MySQL/Redis，并显式运行 `TestMySQLAccountDeletionNativeBridge`。该测试专用 loopback HTTP 服务复用真实路由与账户事务，合成凭据只写私有目录；在提交前后注入 503，让原生 App 用真实钥匙串跨进程恢复。iOS 26.5、18.6 分别通过两个独立进程及后端 race 状态核对，确认受理重放、撤回重放、迟到请求拒绝及其他设备访问不变。仅路由适配在测试端，不修改生产 TLS 或 Gateway 开关；没有部署、真实 Apple 登录或最终身份/媒体/备份擦除。结果见 `docs/evidence/albums-deletion-native-roundtrip-2026-10-14.json`。


照片原生往返：同一脚本显式选择 `--scenario cloud-photo`，以 `albums_live` 构建标签运行 `TestLiveCOSNativePhotoPipeline`。它只对新生成的合成 JPEG 使用真实 COS/CI，控制服务、账户与权益在隔离 MySQL 中；不调用媒体删除或改变部署开关。固定版本原件全量回读、两个预览任务与额度结算均须通过，同时验证原生写入系统副本、原件恢复和重开不重复创建。失败先检查运行目录 intent 与进度，避免盲目重复上传。测试对象保留。

照片发布早于预览完成：原件存在但尚无预览记录时返回 `409 photo_preview_not_ready`，不再误报 `404 asset_version_not_found`；原件缺失、跨账户读取仍拒绝。原生生成客户端需接受 RFC 3339 的可选小数秒。验证及范围见 `docs/evidence/albums-native-cloud-photo-2026-10-14.json`；不代表真实 Apple 登录/购买、远端控制 API 或原件删除验收。


视频原生往返使用同一脚本 `--scenario cloud-video` 与 `TestLiveCOSNativeVideoPipeline`。共享测试桥按类型运行正式原件核验、预览 worker 和额度结算；视频由本机 ffmpeg 合成四秒 2304×1296 H.264/AAC，原生从系统图库读取，经三个 COS 分片上传，等待真实 CI 转码完成后写入系统日常版并恢复原件。两个独立 App 进程核对副本复用和账户隔离；原生额外解码系统视频帧与 PCM 音频。新测试不会删除媒体或改变线上开关。结果见 `docs/evidence/albums-native-cloud-video-2026-10-14.json`。


实况原生往返使用 `--scenario cloud-live` 与 `TestLiveCOSNativeLivePhotoPipeline`。从原生合成的两秒实况导入系统图库开始，完整上传原始双文件，运行照片/视频 worker，原生重建日常配对后走正式分片与配对核验 worker；第二进程优先读取云端固定配对，核对本机已保存副本。原始双文件分别全量回读验哈希，系统日常版与恢复原件均等待 PHLivePhotoView 播放开始/结束。账户及控制服务仍隔离，所有合成媒体保留，无线上变更。见 `docs/evidence/albums-native-cloud-live-2026-10-14.json`。


恢复删除日志时，整体订阅核验记录与各购买链回执在同一事务清除；旧备份中的完成记录不能用于注销收尾。演练会检查清除结果；回滚、重放、其他账户隔离及重新核验均经隔离 MySQL 验证。见 `docs/evidence/albums-recovery-subscription-receipt-2026-10-14.json`。尚未部署，不代表最终身份或灾备擦除完成。


### 在线账户记录清理（默认关闭）

迁移 0063 在独立删除日志增加 `records_erased_at`。媒体结算、访问凭据擦除、完整订阅核验及剩余数据检查通过后，同一事务移除在线账户和订阅记录并写入回执；核验超过一分钟会重新排队。独立删除日志和 Apple 撤销状态保留，故不能将此回执显示为全部数据已删除。重新授权后可创建新媒体账户并恢复同一稳定归属的订阅；旧登录及旧账户重试不能影响新账户。

`ALBUM_ACCOUNT_RECORD_ERASURE_ENABLED=false` 是独立开关，启用还要求订阅核验与改绑开启。Gateway 启动检查新字段，注销 HTTP 入口仍关闭。历史备份清理、撤销状态最终清理、真实 Sandbox 与完整注销验收尚未完成；本次未部署。验证见 `docs/evidence/albums-account-record-erasure-2026-10-14.json`。


身份撤销状态的清理与在线记录开关一致：仅无关联账户、距撤销边界严格超过 10 分 30 秒的记录可移除；仍在使用的身份、近期通知和恢复隔离库保留。注册账户在取得同一身份锁后重新验证断言签发时间，防止此前已验证但迟到的请求在记录清理后生效。迁移 0064 增加到期扫描索引。独立删除日志仍保留，历史备份和日志的最终清理尚未完成；未部署。验证见 `docs/evidence/albums-identity-retirement-2026-10-14.json`。


2026-10-14 混合原生往返：iOS 18.6、26.5 各五个 App 进程完成合成照片、视频和实况上传、提交响应丢失恢复，以及日常版和原件批量系统恢复。第二份系统副本核验后、队列推进前中断，下一进程复用已有副本；整批重放不新增资产。每组四份原始资源按固定 COS 版本全量校验，四份照片预览、两份视频预览和实况配对核验完成，预留容量结清。十个原生阶段、两次 Go race 核对和 vet 全部通过，无失败/跳过。系统视频画面/声音与实况播放通过，合成媒体保留。API、身份与订阅仍为隔离测试，未部署；真实身份购买、原件删除、真机、iCloud 和第三方分享仍待验收。见 `docs/evidence/albums-native-cloud-mixed-restore-2026-10-14.json`。此前上传协调回归见 `docs/evidence/albums-native-cloud-mixed-upload-2026-10-14.json`。


### 云端原件即时存在性检查

`POST /v1/albums/library/versions/{versionID}/original-availability` 使用账户认证、空请求体、禁止查询参数。先校验完整发布清单与已全量验过的固定 COS 版本，再逐个 HEAD 核对版本、大小及在线存储类型；网络访问前后均检查账户、媒体生命周期和六个月保留边界，整个检查最长 30 秒。HEAD 在数据库锁外执行，不重新下载原件，不使用 ETag 或客户端 hash 元数据充当全量校验。返回清单绑定和检查时间，不返回对象路径或下载链接。

此结果只有即时存在性含义，不是防永久清理的预留，也不证明当前 GET 权限；不能单独启用系统原件删除。仍须接入系统副本重读、源版本复核、云端删除并发保护和用户确认。已通过真实 MySQL 并发状态变更验证及既有合成低频对象的 4 次固定版本 HEAD，未部署。证据：`docs/evidence/albums-original-availability-2026-10-14.json`。


## 统一开发身份迁移（2026-10-15，本地实现，尚未部署）

新版测试 gateway 绑定 `cn.tellyouwhat.albums.dev` 与 `api-test.albums.tellyouwhat.cn`，正式 gateway 绑定原正式身份。登录令牌、兑换和撤销的 client_id、账号通知 audience、账号哈希、推送 topic、订阅 verifier/client 使用同一个服务端身份，客户端不能选择。`ALBUM_APPLE_BUNDLE_ID` 可显式填写但必须与部署环境匹配；生产/测试 API 域名混用会被拒绝。

`runtime.env.example` 已改为开发身份，正式部署须明确配置正式域名/身份及其独立资源。已有测试服务使用正式身份及正式商品，其数据库和 Apple 刷新凭据不能直接套用新版开发配置。应保留旧栈恢复能力，为 `.dev` 建新库、登录密钥和独立凭据。历史 SQL、正式账号哈希以及 StoreKit account token 命名空间未更改。

旧 `configure_sandbox.py` / `appstore_probe.go` 针对正式相册 App 的历史 Sandbox 验证，不是 `.dev` 商品配置工具。配置脚本现在要求显式正式 `ALBUM_APPLE_BUNDLE_ID`，并拒绝测试域名，防止把固定正式 ASC ID 写入开发配置。开发身份不继承正式 ASC 商品；本次未注册新商品、未开启销售。production 订阅服务现已分别接收正式身份的 Production/Sandbox 交易与通知，仍需真实 TestFlight 购买、后台运行和跨环境注销验收，完整环境统一尚未完成。


### 生产后端的 TestFlight 购买（本地实现，未部署）

生产订阅配置同时创建两个固定 Apple API/验签器；只有验签成功的交易或通知才会选择对应处理路径。正式接口的网络失败和账号绑定失败不会转入沙箱。数据库既有的 `(environment, original_transaction_id)` 主键分别保存购买链，无需改写历史迁移。

同一账号有正式购买历史时，以正式容量、验证有效期和六个月保留期为准；沙箱的购买、退款或加速到期不会覆盖它们。仅沙箱购买的账号受 `ALBUM_SANDBOX_QUOTA_BYTES` 限制，默认 1 GB，可配置 1～50,000,000,000 字节；实际套餐 SKU 容量不直接变成免费云容量。沙箱到期不生成清理期限，停止新增上传，但已上传原件仍可获取短时下载授权。

共享账号的订阅 revision 继续协调两侧更新，失效的删除证明须重新验证。生产后台为两个购买环境各运行有租约的刷新任务，保留期提醒/删除只通过正式侧处理；沙箱侧更新不写清理提醒。跨环境订阅改绑/完整账号记录擦除还需补齐，因此 Both 配置目前拒绝开启对应开关。开发 `.dev` 免费测试权益、客户端测试容量说明、真实 Apple 购买及后台双环境运行验收仍待完成。

本轮在已有相册测试 MySQL 容器中仅创建随机运行标识限定的独立测试库/用户，Redis 在本机临时运行。验证了购买链隔离、容量投影、正式有效期不受沙箱刷新、沙箱过期后原件读授权及既有订阅通知回归；两个测试运行的库/用户已全部清理。未变更部署、路由或业务数据库。
