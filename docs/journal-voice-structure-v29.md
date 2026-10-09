# Journal voice v29: semantic prose and contextual components

Polish targets and output paragraphs carry a required semantic style. The small ordinary schema supports body, heading1/2/3, ordered/unordered lists, and checklist states, with at most 16 paragraphs and 6000 characters of output. Explicit numbered speech remains a list; a conclusion returns to body. Native numbering is rendered by the App. Existing target styles remain in model input across batches. Prompts retain complete facts and avoid adding emoji by default.

TableSourceContext and TimelineSourceContext carry up to 16 exact source excerpts each, bounded to 6000 characters. These excerpts are content evidence only. Creation commands must still come from pending speech; contextual sources cannot be consumed again or repartitioned as instructions. Shared source validation checks exact anchors, known identities and command/content separation. The App independently verifies the original recording and live document ownership.

Tests cover request construction, structured model input, semantic response decoding, cross-sentence table/timeline validation and rejection of fabricated or missing sources. The voice, development and journaldevserver packages pass race tests and vet. The diagnostic stream test now waits for handler completion before reading its log buffer, eliminating a test-only data race.

App and server must use journal-voice-v29 together. This source milestone does not deploy the running service, push branches, publish TestFlight or install a device build. Emotion UI is not restored.

## 2026-10-04 TestFlight 1.0 (6) 配套部署

已从干净实现提交 `9e9d6691828bd4a2467744c5f8354e82991d30f1` 构建并部署独立
`journal-private-development`。Linux 二进制 SHA-256 为
`26decb287e4ea5e4f4de54900be201856cee3fc458b84e45a2e03c6e889ce094`，
与线上挂载的运行文件一致；认证公网接口确认 `subscription-v2`、`journal-voice-v29`。
现有开发凭证、200 元月度费用保护及状态挂载保留。共享 gateway、worker、admin
的容器 ID、镜像、启动时间与更新前一致，健康和手记正式 readiness 均为 HTTP 200。

通过实际公网会话、一次性票据、WebSocket、模型调用及修订确认，完成三项合成内容验收：

- 明确的第一点、第二点生成两个 `orderedListItem`，完整保留材料、纸笔、九点等事实，
  结尾恢复 `body`，没有主动添加 emoji。
- 先讲“苹果三斤、香蕉两斤”再要求表格，生成两行数据；只消费当前指令，单元格引用前文来源。
- 先讲早上公园、下午图书馆再要求时间线，生成两个事件，保留 morning/afternoon 时间精度。

初次线上表格调用在结构化响应解码阶段失败；合成样本诊断随后得到合法表格，
恢复干净提交后完整线上三项复验全部通过。未放宽解码或来源校验，失败记录保留。
诊断用临时测试源码已移除，没有随服务部署。

证据保存在 `/Users/harborzeng/Documents/JournalAppReleases/1.0-6-20261004-233632/`，
包括实现 SHA、编译摘要、更新前后容器及运行配置、认证版本、模型结果和初次失败记录。
本次没有推送 Git、部署共享正式服务、上传用户录音或完成真机麦克风验收。
