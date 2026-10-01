# Health 年龄资格授权兼容

Health 托管 AI 的年龄与通用文件授权按完整版本对校验，另外仍要求相应 AI 模式和敏感健康处理同意。

| 客户端 | 年龄 scope | 年龄与通用文件版本 | 结果 |
| --- | --- | --- | --- |
| 旧版成人 | adult | 均为 2026-08-24 | 保持原授权 |
| 新版 14 岁以上 | age_14_plus | 均为 2026-10-01 | 新版授权 |
| 混用版本或缺少年龄同意 | 任意 | 不完整 | 不通过 |

`health_eligibility` 只用于服务内部的资格检查，不接受客户端上报。授权数组最多七项，允许新版客户端同时发送新年龄 scope 和撤回旧 adult。旧客户端无需升级即可继续使用原有效授权；adult 的含义仍为成人，不能转换成 14 岁资格。Journal 的资格和文件版本保持原规则，不接受 Health 新年龄 scope 或新通用文件版本。

新客户端须在升级同步时显式发送 adult=false，防止新年龄资格被撤回后回退到旧成人记录。服务不接收生日或此授权用途的精确年龄；资格来自 App 本机确认。

先部署后端兼容代码，再发布 2026-10-01 法律文件和新 App。App 回滚后，新版授权不会被解释为旧成人授权；旧客户端需要重新明确确认原成人资格及旧版文件。后端回滚前必须评估新版客户端，否则旧 API 无法识别新 scope。

API 源为 Contracts/HTTP/PlatformAPI/openapi.yaml，使用 make generate-api 生成共享 Health/Journal bundle 及 Go handler；Health App 仓库同步 Health bundle。服务测试覆盖完整版本对、混用、缺项、撤回和 Journal 隔离。
