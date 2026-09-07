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

服务用户 `health-ai-admin-service` 无控制台登录配置。仅绑定
[HealthAIAdminRead](../../config/iam/ark-admin-read.json) 中五个只读动作。
AK/SK 保存于仓库外的私有文件，仅由明确配置的管理客户端读取。
真实签名请求读取到 95 个模型、49 条开通状态，以及既有 Health Endpoint 的
模型绑定。读取账户全量价格时每页 100 项出现过 `InternalServiceTimeout`；
调整为每页 20 项后完整目录读取通过。

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
测试接入点写入，得到 HTTP 403。服务身份当前仅保留只读策略。临时资源清理
不涉及原有 Health 或 Journal 接入点。

原生写路由保持关闭。后续需要确认火山支持的资源级授权方式；若只能按动作
授予账号范围权限，则需要单独决定是否接受仅由后台白名单限制 Health 范围。
持久命令、超时结果核对、外部变更检测、模型能力与费用上界仍需完整实现和
验证，不能将当前 SDK 适配方法视为已经交付的生产模型切换功能。
