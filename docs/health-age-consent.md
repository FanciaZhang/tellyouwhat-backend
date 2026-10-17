# Health 一般协议与监护人同意兼容

Health 托管 AI 继续单独要求所选 AI 模式和敏感健康信息同意。2026-10-17 起，一般协议允许未满14周岁用户由监护人同意后使用，不再要求客户端声明已满14岁。

| 客户端 | 资格条件 | 版本 |
| --- | --- | --- |
| 旧版成人 | adult 与 privacy_and_terms 均已同意 | 均为 2026-08-24 |
| 已发布十四岁声明 | age_14_plus 与 privacy_and_terms 均已同意 | 年龄为 2026-10-01；一般协议为 2026-10-01 或 2026-10-10 |
| 监护人同意版 | privacy_and_terms 已同意 | 2026-10-17 |

`guardian_consent` 是 Health 专用的监护人声明记录，版本为 2026-10-17；不等同于身份证明或已满14岁的声明。App 在已知未满14周岁时要求监护人明确确认，撤回或缺少该同意时禁止相应处理，并同步 privacy_and_terms=false。年龄未知时提示由监护人操作，不强制填写出生日期。

新客户端发送 adult=false、age_14_plus=false，避免撤回新协议后回退到旧年龄记录；旧 scope 的语义和记录保留。授权数组最多八项，Health 的 guardian_consent 与新一般协议版本不能用于 Journal 或其他 App。health_eligibility 仅为内部资格检查名，不接受客户端上报。服务不接收此授权用途的生日或精确年龄。

先部署后端兼容代码，再发布 2026-10-17 法律页面及对应 App。后端回滚前必须评估已升级客户端，否则旧 API 不识别新增 scope。App 升级后重新确认一般协议，原有 AI 模式选择保留；既有健康数据和订阅不迁移或删除。

API 源为 Contracts/HTTP/PlatformAPI/openapi.yaml；用 make generate-api 生成各 App bundle 和 Go handler。Health App 同步新增 scope 与数组上限。测试覆盖历史版本、监护人记录、缺少一般协议、撤回和应用隔离。
