# Journal 标签与书册建议质量

智能整理通过 `internal/journal/provider/responses.go` 的 `PrepareOrganize` 读取请求冻结的 Prompt 配置，生成结果经结构校验和质量筛选后返回 App。

## 生成规则

- 标签服务于检索：通常 2–4 个，最多 5 个；信息不足允许 0–1 个，不填满上限。
- 不使用“生活、日常、记录、手记、感悟、心情、其他”等空泛标签；人名、地点、机构必须来自原文。
- 同义、同一主题的时间或范围变体只选一个。用户手动标签与已拒绝标签不再生成。
- 优先推荐匹配的已有书册，已收录的书册不再推荐。仅在有明确持续主题、没有适合的已有书册时建议一个新书册。
- 新书册用简短主题名，例如“思考与复盘”“情侣出游”；不带“手记”，不添加“记录、合集”等冗余后缀。

默认 Prompt 位于 `internal/promptconfig/defaults.go`。结构化生成限制位于 `organizationSchema`；公共 `journal-organize-v1` 响应契约保持不变，现有 App 可以接收。

## 返回结果筛选

`internal/journal/provider/organization_quality.go` 仅处理 AI 生成结果：

- 按 App 的大小写、宽度与变音符号忽略规则去重，过滤空泛标签、手动/拒绝标签及无原文依据的人名、地点、机构。
- 合并少量明确的常见主题变体；不按任意后缀合并“旅行/时间旅行”“工作复盘/游戏复盘”等不同主题。
- 清理生成书册名的冗余后缀；清理后名称为空、仍含“手记”或过长时舍弃建议。
- 如新名称对应已有书册，转为已有书册推荐；已收录或存在歧义时舍弃重复建议。保留用户原有书册名。
- 关联标签只保留本次有效标签及用户手动标签，避免已过滤标签再次出现在书册建议中。
- 保留配图建议及计费信息。结构错误、未知书册别名和模型拒绝仍按原有错误/升级重试流程处理。

## 验证与发布边界

日常验证使用 provider、contracts、service、promptconfig、development、gateway 的相关测试与 `go vet`。

`TestLiveJournalOrganizationQuality` 需显式设置 `JOURNAL_ORGANIZE_LIVE_CHECK=1`，使用真实配置的 Lite/Pro 模型和固定合成文本覆盖五种场景：持续复盘、情侣出游、一句短内容、复用带“手记”后缀的用户书册、已经收录且包含手动/拒绝标签。检查未经过筛选的模型输出，不能仅凭筛选后的结果判断 Prompt 合格。通过 `JOURNAL_ORGANIZE_EVIDENCE_DIR` 保留每次生成结果和失败诊断；不得使用私人手记替代合成素材。

可通过 `JOURNAL_ORGANIZE_POLICY_FILE` 载入线上已发布配置的快照；验收仅覆盖整理 Prompt，保留其模型参数和其他写作配置。这属于本地候选验证，不会发布线上配置。

App 的 `JournalOrganizationRealResultTests` 重放真实返回的生成字段，经实际分析服务写入磁盘，再重新打开 SwiftData 存储检查标签、建议和用户书册名。需要 `JOURNAL_ORGANIZE_APP_EVIDENCE_DIR` 与 `JOURNAL_ORGANIZE_APP_WORK_DIR`。Debug 书册 UI 验收入口可通过 `JOURNAL_ORGANIZE_REAL_RESULT` 展示同一真实结果；正式 App 不包含该入口。

线上 Prompt 有独立的已发布版本：部署新代码或修改默认值不会替换已有发布配置。发布时从当时的 current revision 创建新草稿，仅替换 `policy.journal.organize.prompt`，通过后台已有的预检、版本冲突检查及发布流程启用。不得用旧快照覆盖其余配置。

App 不因服务端模型版本改变而批量重做旧手记分析。用户主动“智能整理”可重新生成当前手记；无需重新上传历史内容或修改用户手动名称。
