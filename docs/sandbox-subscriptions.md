# 开发版真实沙盒订阅

开发版使用独立 `.dev` Bundle ID、独立 App Store Connect 商品和测试 API。健康、手记复用正式验单与通知处理链路，但仅接受 Apple Sandbox；不再挂载开发激活服务，历史 development 授权不能通过付费权限检查。生日没有订阅商品，继续使用设备认证和独立服务额度。

| App | 开发 App Apple ID | 开发商品 |
| --- | --- | --- |
| 健康 | 6824089036 | health.premium.subscription.monthly.dev / annual.dev；买断 health.premium.lifetime.dev 只解锁本地高级功能和自行配置 |
| 手记 | 6824089154 | journal.ai.subscription.monthly.dev / annual.dev |
| 相册 | 6824089160 | albums.cloud.50gb.annual.dev / albums.cloud.500gb.annual.dev |

正式商品 ID 不变。测试后端不根据客户端自报权益发放权限。原有 App Attest、隐私授权、预算、配额和付费功能检查继续生效。

健康/手记的当前权益投影仍跟随最近验证的购买环境；新增 `managed_entitlement_environments` 按设备和购买环境保存两份记录。通知仅更新对应交易和环境。迁移保留现有记录；注销通过外键一并清除设备记录。历史购买绑定限制继续生效。

相册 Sandbox 默认实际容量 1 GB，套餐显示以 Apple 商品为准，账户页显示实际测试容量。加速到期停止新增上传，保留已有原件下载，不生成六个月永久清理期限。正式购买历史与保留期限不受沙盒续订或到期影响。

测试部署沿用 `platform_test_deploy.py`，同一运行锁下只更新注册回滚源中的五个测试运维脚本；正式运维脚本、生产服务不随测试更新。失败恢复原脚本与测试服务。新增表是保留旧结构的增量迁移。共享只读签名密钥挂载、配置、源码与数据库检查点进入认证备份，私钥不进入源码或日志。

验收：三个开发目录商品可读；测试后端验单和通知启用；伪造/错环境/错 App/错商品交易被拒绝；模拟权益不能调用付费云功能；购买、恢复、续订、过期和撤销通过真实设备完成。模拟器 StoreKit 文件只用于自动化，不替代 Apple Sandbox 的真机验收。配置完成、后端部署、签名包和真机验收分别记录。

2026-11-01 相册复核：只读 ASC API 确认 `.dev` App 的 V2 Sandbox URL 指向测试 API，Production URL 为空；两个年度商品的中文本地化、中美区价格与供应范围、两周试用可读，状态仍为 `MISSING_METADATA`。这不是设备端 `Product.products` 或购买成功的证据。一次真实 Apple TEST 通知已送达，状态返回 `SUCCESS`；当前通知探针按明确 `.dev` 身份验签通过。公网就绪、未登录拒绝及登录挑战返回通过，但 Apple 登录兑换、实际购买/恢复/续订和容量发放仍待验收；未修改商品目录或服务配置。[本轮证据](evidence/albums-development-apple-readiness-2026-11-01.json)。
