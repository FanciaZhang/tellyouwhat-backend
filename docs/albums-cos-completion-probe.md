# COS 完成上传与清理验证

探针只使用现有相册测试桶和随机生成的 `albums/staging/<owner>/<upload>/photo`。每项上传 17,920 字节合成数据，不使用媒体处理、数据库账户或模拟订阅。正常 CI 不开启网络探针。

后端身份负责上传完成、分片查询和内容核验；清理身份只终止分片、列举和删除具体版本，不为测试扩大权限。创建会话前确认新位置没有版本或分片，持久保存意图；清理前读取并核对完整合成内容。无法确认时保留意图和对象。

## 运行

现有私有凭据位于 `~/.config/tellyouwhat/albums-test/`。准备权限为 0700 的独立证据目录，将其绝对路径导出为环境变量 `ALBUM_CI_LIVE_DIRECTORY`。同一目录不能同时运行两个探针，也不能重跑创建步骤；已有意图先恢复。

```sh
ALBUM_CI_LIVE_PROBE=1 ALBUM_COS_COMPLETION_CLEANUP_PROBE=1 \
go test -tags albums_live ./internal/albums \
  -run '^TestLiveCOSMultipartCompletionPurgeConvergence$' -count=1 -v
```

四种顺序分别检查：完成后清理、完成请求体传输中终止、完成请求已发送后终止、取消尚未传完的完成请求。完成与终止都返回后，查询 `NoSuchUpload`，列举实际对象版本，再核验并删除。

COS 的完成合并可能先返回 200 响应头、后续才返回完整结果；不能仅凭状态码认定完成。[Complete Multipart Upload](https://cloud.tencent.com/document/product/436/7742)。本次实测还观察到完成请求返回 409、对象版本却已存在；服务须保留不确定状态，不能据此重复完成操作或认定没有对象。

## 失败与中断恢复

保留原目录，重新启动独立进程，仅处理其中记录的合成对象：

```sh
ALBUM_CI_LIVE_PROBE=1 ALBUM_COS_COMPLETION_CLEANUP_PROBE=1 \
ALBUM_COS_COMPLETION_RECOVER=1 \
go test -tags albums_live ./internal/albums \
  -run '^TestLiveCOSMultipartCompletionProbeRecovery$' -count=1 -v
```

活动探针持有系统文件锁，恢复进程不能清理它。`ALBUM_COS_COMPLETION_INTERRUPT=1` 仅供中断验收：在 `abort-body-held` 发送部分请求体后写出 `interruption-ready.json`，等待所属测试脚本结束该测试进程；超时则失败。不能用该标志运行日常完整检查。

恢复核对成功后保留证据，再清理自己创建的本地目录。凭据不在该目录，不随测试清理；异常结果不得通过删除意图或重新初始化绕过。

## 验证范围

普通回归包含真实 COS 适配器返回 409 后，业务服务持久保存 `unknown`、禁止重复完成及继续签发分片权限的检查。2026-11-02 的真实 COS 四种顺序、取消、SIGKILL 后新进程恢复、重复恢复和活动锁保护均有记录；最后由独立进程复查全部 10 个本轮合成键，无版本和未完成分片。[证据](evidence/albums-cos-completion-convergence-2026-11-02.json)

这不是账户注销、全部历史写入收敛或备份擦除证明。当前共享备份链路与旧独立相册备份样例的接入差异仍需处理，自动永久清理保持关闭。
