# Apple 开发版 OTA

四个 App 的日常开发使用独立 `.dev` 身份、Debug 编译和 Ad Hoc 签名。TestFlight/商店继续使用正式身份、Release 与生产 API。

`cmd/appledelivery` 提供私有安装页、manifest 和 IPA 下载；不提供 HTTP 上传入口。它不验证 Apple 签名，也不替代各 App 的发布门禁。只有已核对最终 IPA 身份、签名和对应源码验证结果的操作员才能调用 `publish`。候选 JSON 中的布尔字段只是记录，不构成验证证据。

## 构建和本地检查

```sh
go test -race ./internal/appledelivery ./cmd/appledelivery
python3 -m unittest discover -s deploy/tencent -p '*test.py'
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o appledelivery ./cmd/appledelivery
```

Linux 构建只依赖标准库。现有 Dockerfile 也支持 `SERVICE=appledelivery`；`compose.apple-delivery.yaml` 将 HTTP 绑定到 `127.0.0.1:18791`。服务以宿主操作员身份读取只读私有挂载，无出网权限；清理容器单独获得该目录的写权限且不联网。

存储目录必须由操作员所有，权限 0700。首次启动只读服务前，先通过同一镜像执行 `cleanup --data /data` 初始化目录和锁文件。不要以 root 初始化后再改变服务用户。

## 候选和受限链接

候选目录包含原样保留的 `development.ipa` 和 `candidate.json`：

```json
{
  "bundle_id": "cn.tellyouwhat.albums.dev",
  "source_commit": "<完整 40 位源码提交>",
  "version": "0.1.0",
  "build": "1",
  "ipa_sha256": "<最终 IPA 的 SHA-256>",
  "ipa_bytes": 123,
  "state": "verified-local-candidate",
  "ota_published": false,
  "device_installation_verified": false,
  "release_gates_verified": false
}
```

JSON 示例不能直接作为验证通过的候选。发布端必须先执行该项目的实际签名和源码门禁；例如健康预览要求对应源码的 unit lane，不能仅凭归档或局部测试发布。手记保留“不推送”的约束，本地验证不能伪装成远端 CI。

```sh
appledelivery publish --data /private/store \
  --candidate /private/verified-candidate \
  --origin https://install.tellyouwhat.cn \
  --ttl 24h --receipt /private/receipts/installation.json
```

发布检查字节数和摘要，原子保存文件；并发进程共用文件锁。只接受四个开发身份，单包最多 512 MiB，总存储最多 2 GiB。达到上限时报错，不能删除有效候选来腾空间。

回执必须存入 0700 目录，文件为 0600，且不覆盖现有回执。安装地址本身就是访问凭证，只向获准设备的用户交付；不写入公开报告、日志或源码。服务仅保存凭证的摘要，链接最长 24 小时，支持 HEAD 和 Range 下载，单次传输上限 15 分钟且不越过链接有效期。原有 Ad Hoc 描述文件仍限制可安装设备。

## HTTPS 与日志

采用以下两个 Caddy 片段：

- `deploy/tencent/apple-delivery.caddy`：独立安装站点；请求日志丢弃，仅代理 `/d/*`，其他路径返回 404。
- `deploy/tencent/apple-delivery-logging.caddy`：在根 Caddyfile 的全局选项块内导入，排除该站点的错误日志命名空间。仅关闭 access log 不足以保护反向代理失败时的安装链接。

```caddyfile
{
    import /etc/caddy/apple-delivery-logging.caddy
}

# 保留既有业务配置和 import
import /etc/caddy/apple-delivery.caddy
```

先适配并验证完整配置，确认该站点的日志名称为 `apple_delivery`、默认日志排除 `http.log.error.apple_delivery`，且没有 DEBUG/trace 或其他捕获安装请求的日志器，再激活。不要覆盖已有全局日志规则；存在冲突时先合并并重新验收。

依据：[Caddy 全局日志规则](https://caddyserver.com/docs/caddyfile/options#log)、[请求日志规则](https://caddyserver.com/docs/caddyfile/directives/log) 和服务器当前使用的 [v2.11.4 日志实现](https://github.com/caddyserver/caddy/blob/v2.11.4/modules/caddyhttp/logging.go)。`apple_delivery_proxy_test.py` 使用真实 Caddy 和合成 URL 验证失败请求不泄漏，并用其他站点作为日志仍可用的对照。测试绑定临时回环端口，不修改活动配置；本机缺少 Caddy 时明确跳过，不能据此声称代理已验收。

## 清理、回滚和完成条件

`cleanup` 先检查全部清单再删除过期授权；仅当没有有效授权且文件年龄超过两小时才移除 IPA。未知文件、符号链接或损坏清单使清理失败并保留文件。多个有效链接可以共用一个 IPA。

`apple-development` 已接入现有产物所有者机制。部署必须通过 `ops_artifacts.py` 的双锁 producer wrapper，登记当前镜像与匹配配置，保留上一份经过验证的回滚组合。`.operations/apple-delivery.json` 的 `image` 必须与当前 owner 登记匹配；私有数据目录固定为 `.apple-delivery/data`。计划内清理会使用登记镜像执行清理，失败写入运维结果并向上报错；dry-run 不执行删除。

以下都完成后才可报告远程安装可用：已登记部署及回滚、完整配置的加密异地备份、DNS/TLS、日志失败对照、真实 IPA 的 HTTPS 摘要读回、链接到期、计划任务自动清理，以及 iPhone 从外网安装/启动和双版数据隔离。签名材料不得进入安装服务。

当前只完成服务实现、Compose 解析、本机检查和服务器原生 Caddy 日志对照；未部署、未发布任何 IPA。Mac 端项目门禁/签名验证后的发布入口、手记连接文件的受限交付、回滚配置备份及以上手机旅程仍待接通。
