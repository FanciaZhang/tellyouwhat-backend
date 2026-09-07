# Ark 管控验收记录

验证日期：2026-09-07。环境为本机隔离 MySQL、浏览器合成数据及火山北京区域
专用测试 Endpoint。未部署管理服务，未发布生产 AI 配置，未改变生产模型。

## 本地验证

- MySQL 8.4.11 独立实例，数据库 `health_ai_admin_test`：并发发布只有一个胜者；
  相同请求幂等返回原结果；发布审计失败时整个事务回滚；105 条记录完整分页，
  最早版本可直接读取，不受列表页数限制。
- `go test -race`：aiconfig、adminportal、arkcontrol、adminhttpapi、adminui、
  gateway、provider/ark、provider、contracts 通过。云端测试需要显式凭证与
  测试 Endpoint 环境变量，普通测试不调用云端。
- admin、gateway、worker 构建通过。HTTP 测试覆盖未登录、运营角色、CSRF、
  再次验证、独立配置写入开关、签名预览过期及云端状态变化。
- 浏览器使用合成 API 数据及模拟登录完成：保存深度思考/联网草稿、发布确认、
  发布后回显、禁用非饮食决策功能的联网、读取模型开通状态/版本/价格、读取
  原生灰度比例及原/目标模型。此项不证明生产 Passkey 或真实云端发布链路。

## 服务身份与只读 API

服务用户 `health-ai-admin-service` 无控制台登录配置。绑定
[HealthAIAdminRead](../../config/iam/ark-admin-read.json) 中五个只读动作及
[HealthAIAdminRolling](../../config/iam/ark-admin-rolling.json) 中三个账号范围灰度动作。
AK/SK 保存于仓库外的私有文件，仅由明确配置的管理客户端读取。
真实签名请求读取到 95 个模型、49 条开通状态，以及既有 Health Endpoint 的
模型绑定。读取账户全量价格时每页 100 项出现过 `InternalServiceTimeout`；
每页 20 项已成功完成完整读取，但后续复核仍遇到同一间歇错误；不能将缩小
分页视为彻底解决。后台缓存保留上次成功结果并标明过期，首次失败显示不可用。

## 原生滚动升级

官方接口定义来自国内 API Explorer，API 版本 `2024-01-01`：

- [创建](https://api.volcengine.com/api-explorer/?action=CreateEndpointRolling&serviceCode=ark&version=2024-01-01)
- [查询](https://api.volcengine.com/api-explorer/?action=GetEndpointRolling&serviceCode=ark&version=2024-01-01)
- [回退一步](https://api.volcengine.com/api-explorer/?action=RollbackEndpointRolling&serviceCode=ark&version=2024-01-01)
- [取消并归零](https://api.volcengine.com/api-explorer/?action=CancelEndpointRolling&serviceCode=ark&version=2024-01-01)

专用测试接入点 `ep-20260907210357-l5bkt` 未绑定 App 功能。由
`doubao-seed-2-0-mini/260215` 向 `260428` 创建任务
`eprol-20260907210505-vhglx`。云端返回默认 Step=2、WaitDuration=1。
观察到 `Running / 4%`；回退后为 `Reverting / 2%`；取消后为
`Reverted / 0%`。未验证自动推进至 100% 后的最终状态，不假定另有手动全量
或任意设置百分比的接口。全流程未发送真实健康数据或模型推理请求。

## 未通过的权限边界测试

临时策略只允许三个原生写动作，Resource 指定：

```text
trn:ark:cn-beijing:*:endpoint/ep-20260907210357-l5bkt
```

绑定该策略后，服务身份不仅通过范围外接入点的 DryRun，还成功对另一个专用
测试接入点 `ep-20260907211108-6frws` 创建任务
`eprol-20260907211204-w7bfs`。该测试的预期为 HTTP 403，实际为成功，
因此资源边界验收失败。该事实只证明此策略没有达到预期，不能据此断言火山
不支持任何其他资源授权形式。

临时写入策略已解绑，越界测试任务已取消；解绑后使用同一服务身份再尝试
测试接入点写入，得到 HTTP 403。两个权限边界测试接入点及临时策略已删除。
只读核对确认现有接入点仍为原来的 10 个，Health 模型版本维持 260428。

## 后台原生操作与兼容性验证

持久指令通过独立 MySQL 保存后，服务身份在专用接入点完成 mini/260215 到
lite/260428 的原生灰度创建及阶段回退，云端回到 Reverted/0%。测试结束执行
取消清理。此项验证包含数据库指令到 SDK 写调用，不是直接 CLI 写入替代。

另在两个独立接入点分别调用 mini/260428 和 lite/260428。每个模型均验证
minimal、low、medium、high，联网工具配置、32×32 合成图片、1 秒合成静音
音频及严格 JSON Schema 输出，七项均完成并返回对应实际模型名。静音只验证
协议接受和结构化返回，不代表真实语音识别质量验收。

mini/260215 的音频请求被拒绝（InvalidParameter），未将该版本认证为音频
兼容候选。首轮 1×1 图片因最小尺寸限制被拒绝，新版本图片测试改为 32×32。
测试只使用合成内容及测试接入点的短时推理令牌，没有发送用户健康数据。

数据库测试覆盖：并发指令单一胜者、提交重放、价格与审计原子性、创建响应
丢失后的重启不重发、人工核对关联、100% 且模型绑定一致后的完成判定、外部
模型变化、共享接入点拒绝、音频/分档价格上界、过期价格拒绝及实际模型记录。
原生 HTTP 入口覆盖未登录、运营角色、CSRF、再次验证、关闭写入、签名篡改、
跨管理员/动作/目标及过期确认凭据。

MySQL CI 中的审计故障注入使用限定 request_id 的 CHECK 约束，不要求 SUPER
权限或调整 binary logging。生产部署与真实 Passkey 页面到生产云端全流程尚未
执行；没有自动推进至 100% 的云端实测，100% 完成判定使用数据库/云端替身测试。


补充验收：独立接入点第二轮通过持久指令提交取消，云端确认为 Reverted/0%。
页面合成验收完成切换预览、20% 到 18% 的阶段回退、撤销至 0%、操作记录和
实际模型展开，检查请求包含 CSRF 与三个不同幂等标识。页面、登录、确认弹窗
及云端响应均为合成环境，不证明生产 Passkey 操作。

三个最终验收接入点均已停止并删除；服务账号保留已授权的读策略及三个原生
写动作。原有正式接入点未用于写入测试，本功能的生产写入开关尚未启用。

## 2026-09-08 浏览器与发布链路验收

本机隔离 MySQL `ark_browser5_test`、真实 HTTPS 后端和 Chromium 虚拟认证器
完成 WebAuthn 登记与再次验证。页面保存并发布饮食决策的 high/联网配置，刷新
后从数据库回显；读取真实模型版本、账号开通状态及非空输入/输出价格。Apple
Offer 数据及会话存储使用本地替身，AI 数据库和火山 SDK 请求为真实执行。

同一页面经预览、Passkey、幂等指令提交后创建专用接入点的云端灰度，显示
Running/2%，再提交撤销，数据库指令确认云端 Reverted/0%。此项不是模拟登录
或模拟 AI API；仍不代表现有生产账号的 Passkey 登录验收。

测试入口为 `TestLiveAIBrowserServer` 和 `scripts/ai-admin-browser.mjs`。仅显式
提供 `_test` 数据库、服务凭证、`health-ai-admin-validation-*` 接入点和私有
ready 文件路径时启动。脚本需要 Playwright、Chromium，认证器仅属于临时浏览器。
测试失败会回传失败标记，不能作为 Go 测试成功。

服务器上的独立临时目录通过真实 Compose/Docker 验证：凭证只挂载 admin，
UID 65532 可读但不能写，0644 权限被发布预检拒绝。临时目录已清理。30 项
运维测试覆盖凭证缺失、安装失败、版本挂载、发布失败恢复和双向回滚。该检查
没有激活生产容器。现有健康/手记网关、worker、admin 及备份恢复健康检查通过。
