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

`COSObjects` 使用腾讯官方 Go SDK v0.7.75。构造时只接受桶名和地域组成的 HTTPS 官方端点，拒绝重定向；`Check` 只读取版本控制设置，不替用户修改桶。上传地址绑定 staging 路径、Content-Length、Content-Type 和有效期。签名角色与归档角色使用独立凭据，部署必须验证上传角色只有 staging PutObject 权限，无读取原件、修改 ACL 或桶管理权限。

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
