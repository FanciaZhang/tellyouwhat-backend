# Journal v26 独立开发服务交付 — 2026-10-04

此文保留历史。v26 逐项润色不符合用户对正文整理的预期；当前配套实现、部署和真实内容验收见 [v27 叙事整理修复](journal-voice-v27-delivery.md)。

用户授权部署后，使用 JournalApp 的 `scripts/journal-development.py start --backend <配套检出> --no-launch` 更新独立 `journal-private-development`，随后配置并启动手机上的新版 Debug App。

- 部署源码：`2730f84c49c96af58e578f1e9d95859b4e63734e`。
- 运行二进制 SHA-256：`317fa232faf9e65400ec2e280e285dc95bd93334d9f98beb590757dbdcf6b4af`；从准确提交重新编译的摘要一致。
- 公网认证接口确认 `subscription-v2` 和 `journal-voice-v26`。
- 开发凭证未轮换，持久费用目录挂载沿用。共享 gateway、worker、admin 的容器 ID、镜像及启动时间一致；健康与手记正式 HTTPS 就绪接口均为 200。
- App 源码 `099aa719bce594f8ebc6fa1f413120324550cbe0` 完成设备签名构建、严格 codesign 校验、iPhone 17 安装、开发连接配置和启动；应用清单与进程清单分别确认安装与运行。

本次在线合成录音检查未通过。另一录音会话同时处理，测试的 ASR 开启在开发服务费用并发保护处返回 `concurrency_exceeded`，未在完成期限内收齐回执。没有调整并发上限，也没有停止其他录音。这不能记为真实供应商链路或真机麦克风验收通过；此前确定性语音包及 App 聚焦测试通过的结果仍独立保留。

本次未推送 Git、上传 TestFlight 或更新共享正式服务。旧 v25 TestFlight App 不能连接此 v26 开发语音服务；当前手机交付为配套新版 Debug 构建。
