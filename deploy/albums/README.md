# 相册独立测试环境

`cmd/albumgateway` 装配 MySQL 账户、加密 Apple refresh grant、单次登录挑战、可撤销会话、Redis 限流、上传/分片/图库/日常副本和订阅接口。`cmd/albumworker` 执行归档验证和媒体处理。新登录账户容量为零；必须启用并验证相册订阅才允许新增云备份。

`compose.albums-test.yaml` 是独立 Compose 项目：固定 `albums_test` 数据库、专用 MySQL 卷、专用 Redis 和网络、仅 `127.0.0.1:18090` 的 API 入口。迁移全部在这个新库运行。启动服务前检查身份表、上传/分片/派生/实况表与 COS 版本控制；运行中 `/readyz` 检查数据库、Redis 和退出状态。健康检查不等于 worker、Apple、转码或原件恢复通过。

当前 `tellyouwhat-albums-1251995834` 专用于相册开发联调；测试原件/派生前缀全部处于这个测试桶内。正式服务启用前配置独立正式桶与身份，正式数据不得接入此测试栈；不能通过复用这个数据库或桶把测试环境直接当作正式服务。

## 凭据与准备

1. 服务器 `/opt/tellyouwhat/albums-test/` 专用目录，所有者为部署用户，权限 `0700`。核对现有服务内存/磁盘余量、18090 端口和 Docker 子网，默认子网 `172.29.90.0/24`；实际 host gateway 必须与 `ALBUM_TEST_PROXY_CIDR` 一致。
2. `test.env.example` 复制为私有 `.env.compose-albums-test`，只供 Compose 插值；`runtime.env.example` 复制为独立 `.env.albums-test`，只供应用 `env_file`。两个文件均 `0600`；前者的 root 数据库密码不能传给 API/worker。密码分别生成，数据库/Redis 密码使用随机十六进制，避免 DSN/URL/Compose 转义问题。
3. Apple Sign in `.p8` 必须实际授权团队 `T6DD96HY2B` 与 App ID `cn.tellyouwhat.albums`。加密密钥、登录 IP HMAC 密钥各生成独立随机 32 字节，再以标准 Base64 保存到对应文件。应用拒绝同一密钥、非法格式、公共可读或可写文件。须保留加密密钥，换掉它会导致既有 Apple grant 无法解密。
4. `secrets/` 在宿主机设为 `root:65532 0750`，三个文件设为 `root:65532 0440`，让容器 UID/GID 65532 可读。只把该目录挂给 API；worker 不接收 Apple 私钥和账户加密文件。密钥不进入 Git、App、日志或聊天。
5. 腾讯 CAM 新身份分别命名 `albums-test-upload`、`albums-test-backend`，分别绑定 `cam-upload.policy.json`、`cam-backend.policy.json`；保留独立 Secret ID/Key。策略草案尚未应用，需创建访问权限前的确认及实际授权验收。上传身份只写暂存；后端直接读写权限限于开发桶的相册范围，管理分片并查询/提交媒体任务。两份策略没有对象永久删除、ACL/桶管理、其他桶或 CAM 管理权限。若身份另有策略，必须核对总权限。

数据万象[异步任务接口](https://cloud.tencent.com/document/product/460/84789)还要求 `cam:PassRole`。第三份 `cam-media-role.policy.json` 草案只允许后端传递控制台已核实的 `CI_QCSRole`，不会授予直接扮演角色、创建角色或修改权限的能力。**这份策略需要一并明确确认后才可绑定。**现有 CI 服务角色关联了 `QcloudCOSDataFullControl` 等跨桶策略；直接 COS 策略只限开发桶，不代表经此服务角色处理任务也已严格隔离。保留现有角色设置，先用合成媒体核实任务输入/输出的范围及拒绝行为；通过前不启用自动媒体处理，正式数据隔离仍是上线条件，不能为修复 403 授予 `cam:*` 或所有角色的 PassRole。

权限格式及操作依据：[COS 分片上传](https://cloud.tencent.com/document/product/436/65644)、[COS 复制](https://intl.cloud.tencent.com/zh/document/product/436/78078)、[CI 授权粒度](https://cloud.tencent.com/document/product/460/41741)。CAM 文件通过静态范围检查也不证明真实服务会允许/拒绝；真实账号还需校验分片复制、固定版本读取、越界写入和 CI 服务角色范围。派生回收保持关闭，所需版本列举/永久删除权限后续单独配置和验收。

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
