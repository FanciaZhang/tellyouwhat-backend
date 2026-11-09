# 数据保留与恢复边界

AI 任务和临时媒体具有最长 24 小时的服务有效期。任务 API 在准确到期时拒绝读取、取消和幂等重放旧结果，Worker 不再领取过期任务；数据库物理删除由每日 maintenance 执行。物理清理发生在下一次成功的清理作业，不承诺在服务到期瞬间完成。

任务物理清理覆盖排队、运行、成功、失败和取消状态，派发记录通过外键级联删除。对象存储删除失败时保留媒体元数据供重试；数据库清理不依赖对象存储清理成功。清理作业任一部分失败时整体状态为失败，不更新成功记录。

应用层备份只保存数据库结构、App 注册表、管理控制面以及不含设备身份的删除完成摘要。App Attest 身份、授权同意、权益、交易通知、优惠核销、额度、幂等记录、媒体元数据和 AI 任务等 App 用户数据只保留表结构，不进入新备份。备份脚本使用数据表白名单；以后新增的表默认不备份数据，必须经过隐私与恢复评审后才能加入白名单。恢复旧备份不会重新启用用户身份、同意或购买派生状态；客户端重新注册后，StoreKit 交易可从 Apple 重新同步。

配置恢复包可能包含相册等独立业务库的完整检查点；共享主库的数据白名单不证明这些库已清除用户数据，仍须按对应 App 的恢复与删除规则核验。

共享网关的手记录音目录同样属于临时处理数据，不加入配置备份。正常发布和回滚共用本环境的现存目录，保留 `.environment`、任务、执行锁及删除标记；禁止用旧目录快照覆盖它。整机丢失后的恢复只恢复控制面与配置，使用本环境的新空目录，客户端重新注册并重新发起未完成的录音。不要从旧机器备份恢复录音任务、删除标记或设备身份的任意子集；这不是录音内容备份服务，也不新增此类私人数据的异地副本。

应用层备份的恢复有效期为 14 天，以 HMAC 验证后的创建时间判断期限，不使用可因复制而变化的文件时间戳。生成新备份前和每日 maintenance 均独立清理本机到期备份，因此新数据库导出失败不会延长旧备份的恢复有效期。到期备份禁止进入恢复演练。共享 COS 桶按对象最后修改时间计算生命周期，在北京时间零点扫描后异步删除。物理删除以实际回查为准。[腾讯云生命周期说明](https://cloud.tencent.com/document/faq/436/56548)。

本机到期清理以认证清单为入口，先核验所有仍在的密文与恢复包，最后才删除清单；每次删除落盘后，中断可由下一进程继续。未到期时缺少任何成员仍视为损坏，不能借清理规则恢复不完整备份。上传进程持有输入文件共享锁，清理须取得整组文件的排他锁；上传失败、取消或进程退出后释放。无清单的遗留文件、额外硬链接、符号链接、非普通文件或校验中被替换的文件保留并报错，不扩大清理目录。

异地发布先认证并锁定完整输入，将文件摘要传给私有 SDK 进程；每份密文的摘要和字节数读回一致后，才继续下一份，认证清单最后上传。来源变化、缺项、重复或错误大小的回执都不能写入成功记录，调用方后续修改也不能改变已认证的发布范围。上传仍使用普通 PUT；历史版本、未决写入和完整账户擦除尚待完成。[源码与验证](evidence/albums-shared-publication-integrity-2026-11-02.json)。

共享 COS 发布拒绝到达 14 天边界的已认证快照，并要求 COS 禁止同名覆盖。重试遇到 `FileAlreadyExists` 时重新核对已有对象的完整摘要和长度，只有一致才继续，避免改写已有对象的修改时间。其他冲突直接失败。该行为已在固定版本 SDK 的受控 HTTP 联测中验证。[存量检查与联测记录](evidence/shared-backup-retention-audit-2026-11-05.json)。

无法通过完整性校验的备份保留供运维核查，同时继续清理其他已验证的到期备份，并将作业标记为失败。运维须处理校验失败和清理失败告警，不能据此宣称全部副本已经删除。

云数据库快照独立于应用层备份，其保留周期和删除结果必须在云控制台或 API 单独核验。现有旧备份仍可能带回已删除身份和已撤回同意，必须等其按保留周期轮换清除。隔离恢复演练只证明备份完整性，不能授权恢复对外服务；生产恢复只允许使用带有数据白名单清单的新备份，并在恢复后确认所有非白名单表为 0 行，再开放服务。删除完成摘要仅用于相同删除请求的结果确认，不能恢复已删除内容。

运营分层的费用账本只保存调用时权益类别与环境，不保存用户标识，随原费用账本保留。免费设备观察组与 Apple 交易观察关联 App Attest 身份，删除身份时级联清除，并按 400 天使用记录保留窗口清理；这两张表不备份数据。采集起点是无身份的配置元数据，随管理控制面备份。


### Albums collection deletion recovery

Albums' current authenticated deletion checkpoint is v3: account and media fences plus existing collection tombstones. It replays collection deletion only into the quarantined disposable restore database, clears the retired group name and membership, and leaves media and quota untouched. Older v1/v2 checkpoints cannot authorize this restore. Authenticated v2 copies retain their original expiry cleanup rules; v1 retains its review boundary. Production checkpoint code remains bound to its selected source, while the development bundle authenticates all four current modules. This does not enable automatic retention erasure or claim historical-backup/account erasure complete. See [verification](evidence/albums-collection-deletion-recovery-2026-11-02.json).

2026-11-03：账户、媒体及相册删除记录改为在同一 MySQL 只读一致性快照中导出，避免并发注销移除相册墓碑时拼接出不一致的检查点。继续复用原有校验、HMAC、v3 格式和共享备份入口，不新增数据库结构或云权限。

现有 `deploy/albums/backup.py` 增加 `check-ledger`：使用原有数据库/备份目录参数，加 `--deletion-ledger <私有检查点路径>` 和 `--deletion-ledger-sha256 <固定摘要>`。先认证文件及环境，再用只读事务比较三类当前记录；新增、遗漏或冲突均失败并要求重新导出。此核对只证明该次数据库快照一致，后续提交仍可使文件过时；回执明确标记 `point_in_time_only=true`、`authorizes_erasure_or_recovery=false`，不能据此跳过异地最新状态证明或开启最终擦除。检查点文件与数据库保持原样，仅写现有运维审计回执。

真实隔离 MySQL 已验证：导出在账户查询后暂停，另一事务新增账户/媒体删除记录并移除其覆盖的相册墓碑；导出仍完整保留提交前的三类记录。只有 SELECT 权限的账号可执行；旧文件经实际 CLI 拒绝，重新导出后核对通过。加密旧/新快照恢复、删除记录重放及受控异地回归通过。源码尚未部署，完整注销和历史副本最终擦除仍待完成。见[一致性与核对证据](evidence/albums-checkpoint-snapshot-2026-11-03.json)。

核对异地副本时，使用 `backup.py check-offsite-ledger`，传入原有数据库和备份目录参数，以及 `--receipt`、`--receipt-sha256`、`--transfer-bin`、`--cos-config`。命令按已认证回执读取 COS 指定版本，校验密文、快照和删除记录，再与源库同一个只读快照中的三类记录比较。若有遗漏或冲突，命令失败。临时文件沿用备份工作区的租约和回收规则。

本地和异地检查还在同一个只读快照中核对实际数据库名和恢复状态。库名必须与配置一致，且恢复状态必须明确为未隔离。隔离库、缺少状态行或无法读取状态的库，即使删除记录完全一致也会拒绝。检查仍需连接到运维配置指定的源实例。

删除检查点的导出和签名也使用同样的来源校验，数据库名、恢复状态和三类删除记录从同一个只读快照获取。完整备份在清理旧文件与调用 `mysqldump` 前核对来源，再验证实际导出的 `album_recovery_state` 包含唯一未隔离记录。预检后进入隔离的库也无法生成新备份。来源核对基于各次读取时点，恢复切换和最终擦除仍需另行验收。[本轮实测](evidence/albums-backup-publish-source-2026-11-05.json)。

成功回执绑定备份版本和检查点摘要，只证明本次读取时的源库状态。即使通过，之后仍可能发生新的删除。源库不可用、切换前写入尚未冻结或历史副本未核清时，恢复切换和最终擦除仍不能以此回执为依据。

相册业务网关在 `/readyz`、新业务请求和网关内的后台批次开始时读取当前数据库的隔离状态。隔离、状态缺失或读取失败时，业务入口返回 503，后台批次暂不开始，`/livez` 仍可用于确认进程存活。已接收的请求及独立媒体进程需要分别排空，已签发的 COS 直传授权也需要单独收敛。[运行时隔离验证](evidence/albums-running-gateway-recovery-admission-2026-11-05.json)。

独立媒体进程的派生文件清理在领取租约的事务中共享锁定恢复状态行。隔离、缺行或缺表时，领取失败，保留任务、尝试次数和计划，当前批次不会访问提供商。测试用同一队列验证恢复有效标记后仍可正常领取并清理对应版本。该检查不代替切换前排空已取得租约的工作。[媒体清理隔离验证](evidence/albums-derived-cleanup-recovery-admission-2026-11-05.json)。

相册网关只向客户端签发绑定指定上传会话和分片编号的写入授权。原件与实况日常版的整文件 PUT 授权路由已从 HTTP 契约移除。升级时需排空仍可能签发整文件授权的旧网关。历史 `direct_upload_issued` 标记继续阻止缺少在途写入终止证据的永久清理，等待签名过期不能解除该保护。[入口回归与范围](evidence/albums-multipart-only-client-writes-2026-11-05.json)。

接收方领取分享时，在取得账户和分享锁后重新读取时间。到期的请求不占用领取次数，签名或文件信息查询期间到期也不返回结果。这个检查约束新返回的授权，已有 COS 地址仍受原签名有效期约束。[隔离联测](evidence/albums-sharing-expiry-boundary-2026-11-05.json)。

有提取码的分享在公开查询中仅返回是否需要提取码、到期时间和剩余次数。媒体类型和文件清单随领取后授权的短期下载地址返回。共享网关和接收页须使用同一版契约，部署时一起切换。[契约与隔离回归](evidence/albums-share-protected-metadata-2026-11-05.json)。
