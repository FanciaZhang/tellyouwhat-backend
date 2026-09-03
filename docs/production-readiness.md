# 生产就绪与验收记录

核验日期：2026-09-04（北京时间）。后端验证版本：`3c0cd962f71667ee852f0ab3eaff43fe05709987`。

## 部署与外部服务

| 项目 | 已验证结果 |
| --- | --- |
| 自动发布 | `main` 推送触发测试、六个镜像构建发布、腾讯云部署和内部就绪验收 |
| 仓库配置 | 公开仓库；`PRODUCTION_DEPLOY_ENABLED=true`；`PRODUCTION_ACCEPTANCE_STAGE=internal` |
| 实际运行 | Health Gateway、Journal Gateway、Worker、Admin 均通过就绪检查 |
| 数据依赖 | 腾讯云托管 MySQL 与 Redis 私网连通；共享业务使用独立数据库和应用账号 |
| TOS | 合成图片上传、签名下载内容一致、匿名读取拒绝、对象删除全部通过 |
| 方舟 | Health 文字记餐、照片记餐、饮水杯估算；Journal Lite、Pro 均完成真实调用 |
| Apple 配置 | 两个 App 均保存生产和沙盒通知 URL，按各自 API 域名隔离 |
| Apple 沙盒 | 两个 App 均取得 Apple 签名 TEST 通知；对应 App 接收 200、重复接收 200、跨 App 接收 400 |
| 公网入口 | 尚未通过；备案期间 HTTPS 连接失败，Apple 公网投递报告 `TLS_ISSUE` |

[完整 CI/CD 成功记录](https://github.com/FanciaZhang/tellyouwhat-backend/actions/runs/33778948934)
包含真实 MySQL/Redis 集成测试、生成契约检查及部署。服务商连通性使用部署镜像中的
`/servicecheck --models` 验证，不创建虚构生产权益或 App Attest 身份。

Apple 沙盒 TEST 验签不等于完成订阅购买。两个 App 的生产 App Store API 当前返回
401，App Store Connect 仍处于首个版本准备提交阶段；首发后需复验生产 API。
[Apple 工程师关于首发前生产 API 访问限制的说明](https://developer.apple.com/forums/thread/806452)
可用于排查该阶段的 401。

## App 与业务验收

- 后端测试覆盖签名要求、App 隔离、同意校验、免费记餐重试与取消的幂等计费、
  额度上限、重复请求、媒体归属、管理员角色及邀请权限、会话失效和 CSRF。
- 真实 MySQL 验证覆盖迁移隔离、媒体删除失败保留重试、跨 App 相同 key 隔离、
  活跃免费身份保留及仍持有媒体的身份保留。
- 健康五条定向 UI 旅程通过：AI 同意与冷启动、饮食编辑恢复、饮水快速添加与撤销、
  照片草稿恢复与拆餐，以及订阅方案。前四条在 iOS 26.5 验证；订阅方案在 iOS 18.6
  的本地 StoreKit 环境验证。iOS 26.5 的 StoreKit 测试记录存在
  `SKInternalErrorDomain Code=3`，不计为通过。
- 手记十四条定向测试通过：输入与响应契约、配额错误、去重、删除后的迟到结果、
  撤回同意/订阅失效/关闭自动整理后的迟到结果、恢复授权重试、手动标签优先级。
- 健康和手记均完成签名构建、真机安装和启动。此项证明交付成功，不代表真实购买
  或生产 App Attest 旅程通过。

健康的本地测试配置提交为 `e8ed4f6`。手记授权撤回修复及接入说明提交为 `ca7b37e`；
手记保持本地开发，不配置 App 发布流水线。中国大陆月订阅 ¥8、年订阅 ¥64，商品
分别为 `journal.ai.subscription.monthly` 与 `journal.ai.subscription.annual`。

## 运维

| 项目 | 已验证结果 |
| --- | --- |
| 加密备份 | AES-256 加密、独立 HMAC 和摘要校验；备份文件保留 14 天 |
| 隔离恢复 | 真实备份导入临时 MySQL，21 张表及逐表行数一致，临时容器已清除 |
| 云端快照 | 托管数据库每日自动快照、广州地域保留 7 天；2026-09-03 11:45:58 最近一次成功 |
| 定时维护 | 每日 03:05 备份、03:25 清理、每周日 04:05 隔离恢复；北京时间，允许两分钟随机延迟 |
| systemd 实跑 | 备份和清理服务均返回成功，三个 timer 已启用 |
| 自动巡检 | GitHub 每 15 分钟经固定 SSH host key 验证服务、磁盘及备份/清理/恢复的新鲜度，已自动触发成功 |
| 回滚演练 | 当前版回滚到上一版，再恢复当前版；两次真实执行均通过就绪验收 |
| 主机告警 | 腾讯云 CPU、内存、磁盘使用率超过 85%，连续五个一分钟周期触发；已绑定现有主机和通知模板 |

验证记录：

- [回滚到上一版](https://github.com/FanciaZhang/tellyouwhat-backend/actions/runs/33779770831)
- [恢复当前版本](https://github.com/FanciaZhang/tellyouwhat-backend/actions/runs/33779982885)
- [手动健康检查](https://github.com/FanciaZhang/tellyouwhat-backend/actions/runs/33780353286)
- [自动定时巡检](https://github.com/FanciaZhang/tellyouwhat-backend/actions/runs/33781216577)

告警收件配置已核对，未人为制造生产故障验证短信或邮件送达。快照保存在托管数据库
服务中；应用服务器上的加密备份与云端快照是两条独立恢复路径。跨地域备份未启用。
镜像和配置回滚不回退数据库 schema，迁移须兼容前后版本。

## 公网启用验收

正式域名可访问后执行：

1. 检查三个域名的 DNS、可信 TLS 和 `/readyz`，保留已有原生 Caddy 站点配置。
2. 手动运行 `Backend CI/CD`，选择 `deployment=public`；三个公网入口全部通过后，
   将仓库变量 `PRODUCTION_ACCEPTANCE_STAGE` 改为 `public`。
3. 重新请求 Apple 沙盒测试通知，确认 Apple 报告公网成功送达，再完成真机购买、
   恢复购买、权益同步和 Offer Code 兑换验收。
4. 在正式管理后台域名完成管理员 Passkey 注册、登录、邀请与应急恢复验收。
5. 以真实 App Attest 身份验证免费记餐、付费 AI、手记主动整理、撤回同意、删除及
   跨 App 隔离；各 App 首发后复验生产 App Store API。

公网验收命令及运维入口见 [腾讯云部署说明](../deploy/tencent/README.md)。内部就绪、
签名构建和本地测试均不替代以上真实公网旅程。
