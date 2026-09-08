# Ark 管控验收记录

验证日期：2026-09-07 至 2026-09-08。环境为本机隔离 MySQL、浏览器合成数据及火山北京区域
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
`Reverted / 0%`。该用例验证阶段回退和取消；完整自动推进验收见下文。
未假定另有手动全量或任意设置百分比的接口。该用例未发送真实健康数据或模型推理请求。

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
执行；原生自动推进至 100% 的实测证据见下文。


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


## 自动推进至 100% 与实际模型核验

专用接入点 `ep-20260907235501-7hbtn` 从 mini/260428 切换到 lite/260428。
服务身份由持久指令 `39c56dbd-6927-4198-8606-1fb506c41783` 创建原生任务
`eprol-20260908001227-gtxth`。默认策略每分钟推进 2%，创建于北京时间
2026-09-08 00:12:27。01:01:35，执行器观察到 100% 且 Endpoint 已绑定目标模型，
在同一事务把指令从 watching 更新为 succeeded，并写入成功审计。

完成证据从隔离 MySQL 的二进制日志恢复：`binlog.000001`，事务位置
501025–503910，提交时间 2026-09-08 01:01:35.290106。指令 detail 为
“新模型已承接 100% 流量”。08:40 再次通过火山 GetEndpoint 核对，实际绑定仍为
`doubao-seed-2-0-lite/260428`，更新时间为 01:01:28。

测试清理于 01:01:37 调用 CancelEndpointRolling，使原任务查询结果变为
Reverted/0%，没有把已完成切换的 Endpoint 改回 mini。此状态不能用于判断
完成前的灰度比例。验收测试现已避免在确认完成后再次取消。原 Go 进程退出码
未保留，因此不宣称恢复了该进程的 PASS 输出；完整推进结论依据持久事务、审计
及实际云端模型交叉核对。普通管理入口已拒绝取消 100% 的任务。

## 生产主机服务签名

生产主机上的临时容器以 UID 65532、只读凭证挂载执行
`TestLiveSelectedModelPrice`，官方 SDK 成功读取 mini 的账号开通状态和价格。
该测试通过，临时凭证和二进制已删除。服务身份不依赖个人 CLI 登录续期。
生产发布和现有生产管理员的 Passkey 页面验收仍单独记录。

08:43 已停止并删除完整灰度测试接入点。ListEndpoints 核对仅剩原有 10 个
正式接入点，八个 Health 仍绑定 mini/260428，两个手记接入点绑定保持不变。
修订后的 airollout 测试在隔离 MySQL 下通过 race 检查。

## AI 管理交互与动态目录验收（2026-09-08）

- 独立工作树完成三页签概览、参数差异确认、完整目录搜索、版本与价格检查、原生灰度进度和完成后重新切回。
- 浏览器交互测试覆盖 360、390、736、1280 像素，浅色及深色，无横向溢出和脚本错误；覆盖 95 项目录、失败原因、一次 Passkey、重复提交保护、90% 回退及完成后新建反向切换。
- 隔离 MySQL 的竞态测试覆盖动态协议证据复用、仅检查接入点相关功能、文字价格不依赖音频价格、配置变化阻止旧指令、排队任务保留历史能力要求。
- 独立 HTTPS 管理服务使用真实 WebAuthn 和 MySQL 验证参数草稿、服务端预览、再次验证、发布、重载持久化，并读取真实火山完整目录、账号价格、动态兼容检查与 Pro 2.1 原生 DryRun。该验收模式明确拒绝云端写入，不执行线上接入点切换。
- Pro 2.1 `260628`：文字关闭思考、图片深度思考的普通及流式 Responses 与结构化结果通过合成请求实测。DeepSeek V4 Pro GA `260813`：文字深度思考同路径通过。其他版本在选中时按功能检查，不能用这些结果替代所有模型、参数或业务质量验收。
- 新页面的发布状态独立于以上本地与云端只读验收；原生完整生命周期证据沿用前文专用资源验收。

可重复命令：`go test -race ./internal/airollout ./internal/aiconfig ./internal/adminportal ./internal/provider/ark ./internal/arkcontrol ./internal/adminui`，必须设置隔离的 `MYSQL_TEST_DSN`。
浏览器界面测试：`node scripts/ai-admin-ui-test.mjs`，设置 `PLAYWRIGHT_MODULE`、`CHROME_EXECUTABLE`，可选 `AI_UI_SCREENSHOT_DIR`。
真实后台验收沿用 `TestLiveAIBrowserServer` 与 `scripts/ai-admin-browser.mjs`；`ARK_BROWSER_TEST_READ_ONLY=1` 允许读取现有接入点，强制拒绝云端写入，数据库仍必须为隔离的 `_test` 数据库。
