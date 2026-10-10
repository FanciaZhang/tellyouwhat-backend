# 告你相册后端接入

Bundle ID 为 `cn.tellyouwhat.albums`。相册是长期账户媒体业务，不复用 `internal/media` 的 24 小时临时对象，也不以 App Attest 设备 key 作为跨设备账户。

## 当前实现

- 资源清单约束原件、实况配对资源、编辑后显示资源和源版本；服务端逐字节读取封存版本，核对实际大小和 SHA-256。
- 上传任务经过 `uploading → queued → verifying → originals_verified`。客户端提交只入队，不产生备份完成证明；校验失败、缺资源或版本不可固定时不确认备份。
- MySQL 事务先锁定账户，再锁定任务；预占原件与派生资源预算，同账户请求及清单版本幂等，并发不可透支。完成校验后原件预占转为已用，派生预算保留。
- 校验租约支持过期重试，旧租约不能提交结果；客户端只取得短期 staging 写入地址，不能写入原件归档路径。
- `VerificationWorker` 从持久队列分批领取任务，单项失败不阻断其他任务；取消后停止领取，重启后可接管过期租约。轮询观察只提供汇总计数，不向日志暴露对象、签名地址或服务商原始错误。
- 由 OpenAPI 生成的独立 Gin 路由支持创建、查询、申请写入地址、提交任务；要求注入真实账户认证，隔离 Host、App 与账户。请求中的 owner、校验证据以及缺失或 null 的编辑状态均不能被接受。
- `Contracts/HTTP/AlbumAPI/openapi.yaml` 定义接口；当前测试验证合约本身与公开响应，不暴露对象路径、封存版本或租约。

## 接入边界

这些路由尚未挂载公共 gateway；没有生产账户认证器、已配置并验证的 COS 连接或运行中的相册 worker，不能作为已上线备份服务。`originals_verified` 也不表示派生版本已生成或系统压缩副本已验证，不能单独作为删除本地原件的依据。

待完成：稳定账户登录、Swift 客户端接入 App 上传流程和公共路由、App 注册、COS 私有直传与封存、worker 部署装配、失败任务取消和过期对象清理、派生预算结算、CI 派生处理、同步游标与删除标记、恢复、授权分享、回收站及长期索引备份。当前预占不自动过期释放，必须在对象清理与任务状态可证明一致后实现释放流程。

现有 App 注册器包含 Managed AI 特定假设，相册接入时要拆开存储权益与 AI 操作要求，不能伪造 AI 产品 ID 绕过校验。

## 验证

`go test -race ./internal/albums ./migrations` 已通过，包括错误字节/大小/版本、客户端提交不产生备份证明、原件封存后 staging 被覆盖、账户隔离和请求幂等。对象服务测试使用内存替身，不代表 COS 实测。

真实 MySQL 集成测试在设置 `MYSQL_TEST_DSN` 后执行，要求数据库名以 `_test` 结尾；覆盖 20 个并发预占、额度上限、重复创建、租约过期接管、旧租约拒绝、完成幂等及新建 worker 消费持久队列。本机没有 MySQL 配置，因此此项本地跳过，必须以 CI 的实际执行结果为准。

本工作树独立于原后端主目录；未修改主目录用户变更。

## COS 存储适配

`COSObjects` 使用腾讯官方 Go SDK v0.7.75。构造时只接受桶名和地域组成的 HTTPS 官方端点，拒绝重定向；`Check` 只读取版本控制设置，不替用户修改桶。上传地址绑定 staging 路径、Content-Length、Content-Type 和有效期。签名角色与归档角色使用独立凭据，部署必须验证上传角色只有 staging PutObject/UploadPart 权限（分片需另行验证 UploadPart 授权），无读取原件、修改 ACL 或桶管理权限。

封存先读取源版本，再固定该版本进行服务端复制；大于 5 GB 使用分块复制。归档必须返回非 null 的具体版本，后续校验只能读取该版本，响应版本不匹配则失败。元数据和 ETag 不作为完整备份证据。

协议测试覆盖签名作用域和大小、源/目标固定版本、禁用版本控制、HTTP 200 内嵌复制错误及 6 GiB 分块复制（96 个分片均固定源版本）。测试使用 HTTP 替身，没有上传真实用户数据，不代表真实 COS 验收。目标桶配置、IAM 权限验证、生产部署装配、客户端大文件分片直传及失败孤立分片/历史版本清理仍未完成；适配层已改用受控的分片复制：最多并发 2 片、最多 10000 片，失败/取消/合并失败时用独立 15 秒上下文 Abort 本次 upload ID。Abort 失败会上报 CleanupRequired 汇总计数；进程崩溃或服务端不可达仍可能遗留分片，必须完成生命周期与对账清理后才能上线。

接口依据：[腾讯 COS 复制对象](https://cloud.tencent.com/document/product/436/10881)、[Go 预签名 URL](https://intl.cloud.tencent.com/document/product/436/31528?lang=en)。

分片异常路径协议测试覆盖 part 失败、主动取消、合并失败和 Abort 失败，确认不会合并不完整分片，不会把失败当作已备份，且只终止对应任务的 upload ID。

## 独立校验进程

`go run ./cmd/albumworker` 启动独立相册校验程序，不要求现有 AI 的 App 注册、产品或模型配置。部署前先执行既有数据库迁移；进程不自动迁移数据库。启动在 30 秒内检查数据库连通、相册表和 COS 版本控制，失败则退出。SIGINT/SIGTERM 停止轮询，等待正在运行的校验/清理返回后关闭数据库。日志只包含阶段名称和批次计数，不输出 DSN、签名地址、账户或资源标识。

必填配置（值由部署环境注入，不能提交到仓库）：

- `ALBUM_DATABASE_DSN`
- `ALBUM_COS_BUCKET`、`ALBUM_COS_REGION`
- `ALBUM_COS_ARCHIVE_SECRET_ID`、`ALBUM_COS_ARCHIVE_SECRET_KEY`
- `ALBUM_COS_UPLOAD_SECRET_ID`、`ALBUM_COS_UPLOAD_SECRET_KEY`
- `ALBUM_MAX_ORIGINAL_BYTES`、`ALBUM_MAX_DERIVED_BYTES`（正整数；由上传产品限制确定，不能溢出）

可选的临时凭据 token 分别为 `ALBUM_COS_ARCHIVE_SESSION_TOKEN`、`ALBUM_COS_UPLOAD_SESSION_TOKEN`。当前静态配置不会自动刷新临时凭据，不能将短期 token 当作长期运行配置。任务有效期 24 小时、校验租约 30 分钟、每批最多 10 项、轮询间隔 5 秒；超大资源需通过真实吞吐验证租约预算。

程序可构建，但未加入生产 Compose/发布矩阵，尚无真实云端配置和运行验收；部署生命周期及健康检查集成仍待完成。

## Swift 控制客户端

合约包新增 `AlbumAPI` 产品，由既有 Swift OpenAPI 构建插件生成 Client 和类型。桥接层向 App 传递生成后的 method/path/headers/body，账户认证由 App 发送层提供；没有硬编码真实 API 域名或伪造登录。控制 JSON 与媒体文件传输分离。

本地 `swift test --package-path Contracts/HTTP` 的 12 项测试全部通过（相册 4、健康 6、日记 2）。新增测试验证创建请求只有 requestID/manifest、edited 必须存在且 false 会被保留、申请写入地址/提交校验无 body，以及 queued 响应不会被转成原件已验证。客户端尚未接入 iOS 上传界面或真实登录。

已读取 CI `38071817152` 的完整成功结果和日志：`418eb15` 的真实 MySQL 事务测试、队列执行器与 COS 分块复制协议测试通过；verify 和六个容器构建全绿，deploy/verify-public 按 PR 条件跳过。这不覆盖后续本地提交，需由新的 CI 验证。

## Apple 身份断言校验

`AppleIdentityVerifier` 校验 RS256 签名、固定 Apple issuer、唯一 audience `cn.tellyouwhat.albums`、有效期/签发时间、非空 subject 和服务端预期的 SHA-256 nonce。断言签发需在 10 分钟内，时间检查允许 30 秒时钟偏差；不接受客户端自报的账户 UUID，也不依据邮箱或姓名建立账户归属。

公钥只从 `https://appleid.apple.com/auth/keys` 读取，不信任 JWT 内的 jku/x5u。HTTPS 请求禁止重定向、限时 10 秒、响应限 64 KiB；公钥缓存 1 小时，未知 kid 的刷新至少间隔 30 秒，过期缓存刷新失败时不继续验证。密钥限制 RSA/签名用途/RS256，拒绝重复 kid 和无效 modulus/exponent。

密码学测试使用本地生成的 RSA 密钥真实签发/验签，覆盖受众、签发方、时间、nonce、伪造签名、算法替换、轮换、并发未知 kid、重定向、超大响应和过期缓存。不包含真实用户 Apple 登录。

此模块只返回已验证的 Apple subject，不签发 App 会话。持久一次性挑战、authorization code 换取/核验、稳定账户映射、会话撤销/轮换及 App 登录界面仍待接通；在这些步骤完成前不能把断言校验器直接当作可上线登录接口。

依据：[Apple 验证用户](https://developer.apple.com/documentation/signinwithapple/verifying-a-user)、[Apple 登录认证流程](https://developer.apple.com/documentation/signinwithapple/authenticating-users-with-sign-in-with-apple)。

## Apple 原生授权码交换

`AppleCodeExchanger` 使用部署提供的 Team ID、Key ID 和 P-256 私钥生成 5 分钟 ES256 client secret。client_id 固定为相册 Bundle ID，端点固定为 Apple `/auth/token`；不接受客户端覆盖地址、受众或 redirect_uri。请求限时 10 秒、禁止重定向、响应限 64 KiB。

交换前先验证客户端身份断言，避免无效断言消耗授权码；交换后再次验证 Apple 返回的 id_token，要求 subject 和本次 nonce 与先前身份一致。服务商错误和未知结果均不自动重试一次性授权码。返回的刷新令牌不会被 JSON 序列化，格式化及结构化日志均脱敏；后续账户存储必须加密保管它，用于校验和撤销，不能直接作为 App 会话。

测试使用真实 ES256 客户端签名与 RSA 身份签名，核对请求表单和 client secret 的 issuer/audience/subject/kid/有效期，拒绝错用户、错 nonce、过期返回、错误曲线、重定向、超大响应及服务商错误。无效客户端断言不会请求 token 端点；网络错误只发一次。尚未使用真实 Apple 授权码联调。

此组件仍需接到持久一次性挑战、账户事务和会话模块，不能单独视为登录完成。协议依据：[Apple Token validation](https://developer.apple.com/documentation/signinwithapplerestapi/generate-and-validate-tokens)。

## 持久一次性登录挑战

迁移 `0016` 增加独立的 `album_login_challenges`。挑战包含随机 UUID、独立的 256 位客户端 proof、Apple nonce 和 5 分钟有效期；只存 proof 的 SHA-256，不存原始 proof。返回的 nonce 已是 SHA-256 十六进制值，iOS 应直接赋给 Apple request.nonce，不能再次哈希。日志格式化对挑战整体脱敏。

`Consume` 用数据库条件更新及事务保证未使用、未过期且 proof 哈希匹配时才消费；错误 proof 不会烧掉合法挑战。`VerifyAppleLogin` 先消费挑战再交换 Apple 授权码。交换失败或网络结果未知时要求重新发起登录，不重试可能已消费的授权码；当前没有透明恢复这一登录尝试的承诺。

服务测试覆盖哈希存储、错误 proof、重放和过期边界，以及网络结果未知时不会重复兑换。新增真实 MySQL 测试使用 20 个独立服务实例竞争同一挑战，要求仅一者成功，并验证新实例仍拒绝已消费挑战。此 SQL 测试本地无数据库时跳过，必须等新 CI 执行。

挑战尚未公开为 HTTP 接口；公开前需接入发行频率限制、过期记录清理和账户/会话事务。该模块不创建账户，不下发 App 登录令牌。

## 稳定账户与 Apple 授权存储

迁移 `0017` 增加相册账户和 Apple 授权表。账户身份由固定 issuer、Bundle ID、Apple subject 的哈希唯一映射；重装或换设备不生成另一账户，不以邮箱/设备 ID 识别用户。只有服务端完成断言和授权码核验得到的内部 `AppleGrant` 可进入此事务。

每个不同刷新授权单独保留，以 AES-GCM 加密，附加认证数据绑定账户、身份哈希和授权 ID；部署必须提供相册专用加密密钥。相同授权重复登录复用记录，已撤销授权以及 disabled/deleting 账户拒绝登录。清理过程仍可读取并解密停用授权，以便向 Apple 撤销；该方法不对客户端开放，也不能充当 App 会话认证。

首次登录创建零容量额度记录；再次登录不会覆盖订阅额度。MySQL 集成测试覆盖 10 次并发登录、服务实例重建、跨设备授权保留、额度不重置、跨账户读取拒绝、密文篡改及撤销/停用拒绝。本地没有 MySQL，数据库测试需在 CI 验证。加密绑定测试与本地全仓 Go 测试通过；真实 Apple 登录、密钥轮换、App 会话和公开 HTTP 登录仍未完成。

## App 会话基础

迁移 `0018` 和 `MySQLSessions` 增加独立于 Apple 凭据的 256 位随机 Bearer 令牌，数据库仅存 SHA-256。服务端验证后的账户/授权才能签发 30 天会话；签发事务按账户、授权顺序锁定并检查状态，不接受客户端声明账户归属。

认证器实现现有 `AccountAuthenticator`，每次请求读取会话到期/撤销、账户 active 和 Apple 授权未撤销状态；不使用可能滞后的身份缓存。Authorization 必须是单个 Bearer 凭据，拒绝重复头、逗号拼接、非规范 token，也不从 URL 读取。退出只撤销当前令牌且幂等，不影响另一设备。令牌格式化日志脱敏，只有登录响应 JSON 显式返回它。

本地令牌格式、哈希、日志脱敏与相册 race 测试通过。真实 MySQL 生命周期测试接入账户测试，覆盖服务重建、到期边界、退出后拒绝、其他设备保持登录、账户停用及 Apple 授权撤销后拒绝认证/签发；本地无 MySQL 时跳过，需下一次 CI。数据库状态撤销已接通，接收 Apple 服务端撤销通知的入口尚未实现。

此模块尚未挂接公开 API。会话续期/轮换、登录流程编排、iOS Keychain、设备列表及全部设备退出仍待实现；30 天有效期不是最终的持续登录体验。撤销后已开始的请求和此前签发的 COS 短期 URL 不会被此数据库检查追溯取消。

## 登录流程编排

`LoginService` 将挑战消费、Apple 授权码交换及双断言校验、稳定账户注册、App 会话签发按顺序串联。请求只含挑战 ID/proof、授权码与身份断言，不接收 account ID/Apple subject；请求日志脱敏，向调用方统一返回登录失败，不透出服务商或数据库内部错误。旧的仅交换授权码辅助入口已移除，避免出现两套流程。

任一步失败都不执行后续步骤、不返回会话。挑战已消费后发生失败，必须重新发起 Apple 授权；若账户已提交而会话失败，下次新授权仍回到同一持久账户，不删除账户或重置容量。流程测试覆盖各阶段失败、顺序及数据来源、错误 proof 不消耗合法挑战、重复请求不触及 Apple。真实签名验证配合 HTTP 替身覆盖发送授权码后断线，确认仅请求一次且账户/会话均未调用。

公开 HTTP/OpenAPI 路由、分布式频率限制、会话续期以及 iOS 登录/Keychain 尚待接入。本组件没有自行暴露网络接口。

## 登录限流组件

`RedisLoginLimiter` 用 Lua 原子计数，挑战与登录分别计费；同一地址在多实例/实例重建后共用 Redis 额度。窗口从首次请求开始，拒绝请求不延长窗口；达到上限返回剩余等待时间。Redis 超时、异常或计数缺少有效过期时间时拒绝放行，调用限时 2 秒。容量与窗口需部署显式配置；这不是全局防攻击或边缘流量防护的替代。

Redis key 使用独立 32 字节部署密钥对地址及操作做 HMAC，不存原始 IP；IPv4-mapped 地址归一化，IPv6 按 /64 聚合。调用方必须从可信连接/明确配置的代理获取客户端地址，不能直接采用任意 X-Forwarded-For。当前尚未挂到 HTTP 登录入口。

已用本机一次性 Redis（仅 Unix socket、无 TCP、无持久化）执行真实集成测试：30 个实例并发仅 7 个获准，验证 TTL、窗口到期重置、缺失 TTL 拒绝；相册/迁移 race 与全仓 go vet 通过。临时 Redis 已停止。第一轮单测的 IPv6 测试地址写成超长字段而失败，修正为合法地址后上述测试通过。普通地址解析、操作隔离和 Redis 不可用时拒绝测试也通过。

## 登录 HTTP 与合约

OpenAPI 新增 `POST /v1/albums/auth/challenges`、`POST /v1/albums/auth/apple`、`POST /v1/albums/auth/logout`、`GET /v1/albums/account`。`NewHTTPRouterWithLogin` 要求登录服务、限流器和会话撤销器完整配置。仅前两个精确 POST 路径允许未登录访问，上传、账户与退出仍通过现有账户/App/Host 验证。旧构造器继续要求认证，不会意外开放登录。

所有响应 `Cache-Control: no-store`。挑战与登录先限流，429 返回向上取整的 Retry-After；Redis 不可用返回 503。登录仅接受 JSON，body 限 80 KiB，拒绝未知字段及尾随 JSON；挑战和退出要求空 body。退出只撤销本次 Bearer 凭据，返回 204。默认不信任任何代理，只有配置的 TrustedProxies 才能影响客户端 IP；Host 不采用 X-Forwarded-Host。

HTTP 测试覆盖未登录挑战/登录、账户字段注入、重复登录、超大/尾随 JSON、限流/Redis 故障时不触发 Apple、伪造转发地址及错误 Host、配置代理、保护上传/账户/退出、当前令牌撤销。相册/迁移 race、全仓 go vet 和全仓 Go 测试通过。此路由构造器尚未接到生产 gateway 或部署环境，iOS 也未实际登录。

此前 `86a4c4f` 的 CI `38074465739` 已完整成功，日志明确显示 `TestMySQLAppleAccountStableAcrossConcurrentLogins` 与 `TestMySQLLoginChallengeConsumedOnceAcrossServiceInstances` 通过。该结果不覆盖此后会话、限流和登录 API 的改动，需新 CI。

Swift 合约包使用独立 XcodeMaintenance 目录重新生成/构建并执行 14 项测试通过（相册 6、健康 6、日记 2）。新增验证登录 body 只含四个证明字段、会话响应解码，以及退出请求空 body/204 空响应；生成 Go 文件重复生成 SHA-256 一致。运行 `albums-login-contract.ZyZnTz` 已检查结果并标记 success。仅为客户端合约测试，未接入 iOS 实际网络登录。

## COS 客户端分片适配

新增按服务端资源大小推导的分片布局，通常 8 MiB，超大资源提高片长以保持不超过 10,000 片；片长上限 5 GiB，末片按剩余实际字节计算。范围/编号越界、零长度和极端溢出拒绝。依据：[COS Upload Part](https://cloud.tencent.com/document/product/436/7750)；分片可以乱序上传，但服务端完成列表须按编号连续完整。

`InitiateStagingMultipart`、`AuthorizePart`、`CompleteStagingMultipart`、`AbortStagingMultipart` 仅操作合法 staging 键。单片签名使用独立上传角色，绑定 uploadId、partNumber、Content-Length 和 Content-Type，最长 10 分钟；不接受任意客户端范围。完成时从 COS 分页查询，检查 uploadId/key、连续编号、片长、数量、ETag 和分页推进，使用该列表合并并要求返回明确版本号，不信任客户端 ETag。

适配器显式把 SDK 自动重试次数设置为 1，包含既有 COS 路径；瞬时读取失败也交由上层重试。初始化/合并请求失败或结果缺少必要确认时带 ErrCOSMultipartUnknown，需持久对账，不应盲目重建或标记失败后放任用户重复初始化。合并所得只是一份 staging 对象，仍须完整版本读取、大小/SHA-256 校验和归档才能形成备份证据。

测试覆盖精确字节布局、签名查询/长度约束、错误作用域、分片缺失/尺寸错误/会话错误/坏游标、1,001 片分页、初始化/取消目标，以及初始化和合并未知结果不自动重试。HTTP 使用协议替身，尚无真实 COS 调用。真实 IAM 必须允许上传角色仅对 staging 执行 PutObject/UploadPart，初始化/查询/合并/取消留在服务端。

当前仅适配器，尚未新增持久分片会话或开放 API。服务层必须先持久绑定账户、上传任务、资源和 provider uploadId，持久记录初始化意图并处理未知结果，才可向客户端发授权。孤立会话回收、iOS 分片缓冲和端到端续传仍待实现。首轮测试修复 Go 正则重复次数上限导致的初始化失败，分页测试补齐符合 SDK 校验的 ETag 响应后通过。

续传查询 `ListStagingMultipartParts` 复用完成阶段的分页校验，允许尚未传齐的稀疏编号，但拒绝重复、倒退、越界和长度不符的分片。对外仅返回编号与字节数；完成阶段额外要求全部分片齐全，提交 XML 仅含编号和 ETag。新增测试覆盖跨页缺片、空会话和异常观察；相册 race 与全仓 vet 通过。持久会话及 HTTP 入口仍待实现，该查询尚不能由 App 调用。

## 持久分片初始化意图

迁移 `0019_album_multipart_intents.sql` 为每个上传任务/资源建立唯一初始化记录。`MySQLUploads.BeginMultipart` 按既有账户→上传锁顺序检查归属、资源清单、父任务状态和有效期，在事务提交后才返回一次性的 created=true。并发调用、实例重建及未知提交结果的重试都不能再次取得初始化资格。

`ResolveMultipart` 用原始 intent UUID 绑定 COS 会话；无确认结果记为 unknown，迟到的有效响应可解析 unknown，但 ready 不能换成另一 provider ID 或退回 unknown。过期后仍保存迟到观察，方便后续回收；这不允许过期授权，签发分片时必须再次核对父任务。初始化记录不按超时自动重置，尚需对账与孤立会话回收。

本机使用独立、仅 Unix socket 的 MySQL 8.4 测试实例执行真实迁移/重复迁移以及上传事务测试（race 开启）通过。新增场景涵盖 20 个仓库实例并发仅一个创建者、重建实例读回 unknown、不存在的资源、跨账户读写拒绝、错误 intent、迟到响应、绑定不可覆盖和到期拒绝。相册/迁移 race 与全仓 vet 通过。当前尚无调用 COS 的服务编排或 HTTP 分片 API；本表的 ready 仅代表 provider 会话已绑定，不是媒体已上传/备份。

## 分片初始化、授权与续传服务

`MultipartService` 将真实 MySQL 初始化记录与 COS 适配器连接。Initialize 先读取账户拥有的未过期上传/资源并核对分片布局，再提交初始化意图；只有 created=true 才调用 COS。已绑定会话可重用，initiating/unknown 返回待对账，不重新发起初始化。响应不暴露 provider ID，只返回资源和分片尺寸。

客户端取消后，服务仍以独立 5 秒上限保存刚收到的 provider ID；数据库确认失败不返回可用会话。保存成功后再次核对父任务/取消状态。Part 与 Progress 只读取已持久绑定，不能隐式初始化；对象键、资源长度、会话和有效期全部来自服务器记录，无客户端自选 key/uploadId 接口。

新增服务测试覆盖持久化先于外部请求、已绑定重试、初始化未知结果、数据库提交前/后失败、客户端断开时仍保存响应、初始化过程中到期，以及跨账户、越界编号、错误资源绑定和未确认会话不触及签名/查询。相册/迁移 race、全仓 vet 通过；真实 MySQL 测试新增读回以及跨账户/缺失资源查询拒绝并通过。尚未开放 HTTP 路由，分片合并的持久状态、未知结果对账、取消/清理和 App 发送仍待实现；没有真实 COS 上传验证。

## 持久分片合并与安全重试

迁移 0020 独立记录每个资源的合并操作 UUID、状态和明确对象版本，不改变初始化记录的含义。ClaimMultipartCompletion 按账户/父任务锁串行化，只允许一个 completing；已 assembled 重试返回原记录，unknown 不因时间流逝重新发起。只有证实未发送外部合并的 retryable 才能生成新 UUID；旧操作的迟到写入被拒绝。

MultipartService.Complete 先持久领取，再调用 COS 的服务端列表/合并；COS 在合并发送后产生的错误均带 Unknown，服务保留 unknown。缺片、列表查询失败等合并前错误允许重试；未分类错误保守保留 unknown。客户端断开不丢弃已收到的对象版本，使用独立 5 秒上下文保存。成功仅返回 assembled，不自动提交备份或生成删除原件凭据。Part 在读取到 completing/unknown/assembled 时拒绝新授权；已经发出的短期授权仍可能有效，完整哈希及不可变归档校验仍不可省略。

真实 MySQL 8.4 测试覆盖迁移重放、20 实例并发唯一合并许可、跨账户读取、已知前置失败重试、旧 operation 隔离、unknown 无自动重试、迟到明确版本、已完成版本不可覆盖及过期拒绝。服务测试覆盖成功重放、前置/未知/未分类错误、缺版本、数据库写入失败、客户端取消后版本仍持久化，以及合并状态下拒绝分片授权。相册/迁移 race 和全仓 vet 通过。未知结果的主动 COS 对账、取消清理、HTTP 合约及 App 上传仍待实现。

远端 CI 38078438699 已完整成功（提交 50c92bc），部署/公开验证跳过；不覆盖此后持久会话与服务编排提交。后续批量推送需新的 CI 结果。

## 合并未知结果的内容核对

`COSObjects.VerifyStagingResource` 先 HEAD 获取明确版本和大小，再固定该版本 GET，以流式 SHA-256 核对完整资源，最多多读一个字节识别超长响应；不使用 ETag 或客户端 metadata 代替内容证据。读取限时 15 分钟并服从调用方更短的取消/截止时间，不创建整文件本地副本。

`MultipartService.ReconcileCompletion` 作为可信 worker 操作，仅处理账户/资源绑定正确的 completing/unknown；核对成功才用原 operation UUID 写入 assembled。允许过期任务记录迟到成功，但不续期额度或授权，不把父任务改成 originals_verified。缺对象、错误版本、大小/内容不符、断流及数据库失败均不重置为 retryable；未找到对象也不能证明外部合并一定失败。已 assembled 重放不重复读取媒体。

新增协议测试验证固定版本读取、HEAD/GET 大小、缺版本/版本漂移、同长度错误内容、截断/额外字节、读取失败、响应关闭及非法输入不访问 COS。服务测试覆盖 unknown/completing/过期恢复、内容/数据库失败保持记录、跨账户拒绝及已完成重放。相册/迁移 race、全仓 vet 与 diff 检查通过；未实际连接 COS。本方法尚未挂入轮询 worker，需先加数据库认领、退避与读取预算，避免多实例重复下载大视频；初始化未知结果与孤立分片清理仍需单独处理。
