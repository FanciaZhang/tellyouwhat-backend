# Apple 开发版 OTA

四个 App 的日常开发使用独立 `.dev` 身份、Debug 编译和 Ad Hoc 签名。TestFlight/商店继续使用正式身份、Release 与生产 API。

`cmd/appledelivery` 提供私有安装页、manifest 和 IPA 下载；不提供 HTTP 上传入口。它不验证 Apple 签名，也不替代各 App 的发布门禁。只有已核对最终 IPA 身份、签名和对应源码验证结果的操作员才能调用 `publish`。候选 JSON 中的布尔字段只是记录，不构成验证证据。

## 构建和本地检查

```sh
go test -race ./internal/appledelivery ./cmd/appledelivery
python3 -m unittest discover -s deploy/tencent -p '*test.py'
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o appledelivery ./cmd/appledelivery
```

Linux 构建只依赖标准库。现有 Dockerfile 也支持 `SERVICE=appledelivery`；`compose.apple-delivery.yaml` 将 HTTP 绑定到 `127.0.0.1:18791`。服务以宿主操作员身份读取只读私有挂载，使用专用 bridge 网络；应用不发起外部请求，该网络不提供出站防火墙。清理容器单独获得该目录的写权限且不联网。当前主机的 internal 网络不会生效主机端口映射，不能用配置解析通过代替实际连通性。

存储目录必须由操作员所有，权限 0700。部署入口会先通过同一镜像执行清理命令初始化目录和锁文件，再启动只读服务。不要以 root 初始化后再改变服务用户。

## 服务部署

操作员从准确提交的干净源码构建 Linux/amd64 静态二进制。将以下文件放入宿主 `.apple-delivery/releases/<完整源码 SHA>/`，父目录均为操作员所有、0700，文件均为 0600：

- `service`：该提交的 `cmd/appledelivery` 构建结果。
- `Dockerfile`：该提交的 `deploy/tencent/apple-delivery.Dockerfile`；镜像仅复制此二进制，不联网构建。
- `compose.apple-delivery.yaml`、`apple-delivery.caddy`、`apple-delivery-logging.caddy`：原样复制对应源码文件。
- `selection.json`：`source` 为完整提交，`image` 为 `tellyouwhat-apple-development:<提交>`，`origin` 为安装域名；`files` 将上述五个文件名映射到各自 SHA-256。

安装已审阅的运维脚本后，使用：

```sh
python3 deploy/tencent/apple_delivery_deploy.py --source <完整源码 SHA> --source-run <本次运行标识>
```

入口通过现有 owner wrapper 取得 operations/deployment 双锁，检查原生产路由和自身回滚登记，构建/检查不可混用的镜像，生成匹配配置，初始化存储并检查就绪，随后登记和清理。构建输入的 tar 中将二进制权限设为 0555，宿主原文件保持 0600，兼容当前 legacy builder，无需 BuildKit。服务启动或状态写入失败会恢复前一镜像及对应配置；首次失败仅移除自身 Compose 项目，不删除存储卷或数据。清理或备份失败仍需处理，不能因服务健康就宣称部署完成。

当前及上一版的二进制、镜像构建文件、Compose、运行配置、摘要清单、owner 登记和恢复状态均纳入加密配置备份，恢复验证按认证清单逐文件核对。安装包与临时访问凭证可重新生成，不纳入该备份。服务部署本身不会配置公开路由或发布 IPA。

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

请求 Host 接受配置域名的大小写等价形式和显式 HTTPS 默认端口 443；其他域名及端口仍拒绝。Manifest 和下载地址始终使用配置中的规范 HTTPS 源，不能由请求 Host 改写。

手记的首次开发连接独立交付，不进入 IPA 或安装页。先用当前手记源码的 `journal-development.py export-profile` 在线验证并生成私有连接文件，再对已经上传且摘要匹配的手记开发候选执行：

```sh
appledelivery journal-connection --data /private/store \
  --candidate /private/verified-journal-candidate \
  --journal-connection /private/connection.json \
  --origin https://install.tellyouwhat.cn \
  --ttl 15m --receipt /private/receipts/connection-link.json
```

仅接受本人所有、0600、最多 1 KiB 的固定测试域名连接文件，链接最长 15 分钟。连接内容通过 AES-256-GCM 封装在单独下载凭据中，绑定安装源、候选摘要及有效期；回执仍只写受限文件。HTTP 仅返回带下载文件名和禁止缓存响应头的 JSON，安装页面不显示这条链接或其中令牌。客户端继续校验域名、当前协议及在线权限，成功后才存入独立钥匙串。

服务器只在私有安装缓存保存一份 0600 的封装密钥 `.journal-connection-key`，不保存连接文件正文，也不把密钥挂入其他服务。链接到期直接失效，不产生需要回收的连接文件。密钥和短时链接与安装缓存一起可再生，不属于业务数据备份；丢失缓存后重新发放链接。已有安装授权格式保持兼容，旧服务对连接下载路径返回 404，恢复当前服务后可继续解封尚未到期的链接。兼容性已用实际旧版和当前二进制、合成传输数据验证，见 `evidence/apple-delivery-connection-compatibility-2026-10-16.json`；真实公网连接文件与手机导入仍须单独验收。

## HTTPS 与日志

采用以下两个 Caddy 片段：

- `deploy/tencent/apple-delivery.caddy`：独立安装站点；请求日志丢弃，站点级 `log_name apple_delivery` 在所有处理器前固定日志名称，仅代理 `/d/*`，其他路径返回 404。
- `deploy/tencent/apple-delivery-logging.caddy`：在根 Caddyfile 的全局选项块内导入，排除该站点的错误日志命名空间。仅关闭 access log 不足以保护反向代理失败时的安装链接。

```caddyfile
{
    import /etc/caddy/apple-delivery-logging.caddy
}

# 保留既有业务配置和 import
import /etc/caddy/apple-delivery.caddy
```

先适配并验证完整配置，确认该站点的日志名称为 `apple_delivery`、所有站点请求在处理前无条件绑定该名称、默认日志排除 `http.log.error.apple_delivery`，且没有 DEBUG/trace 或其他捕获安装请求的日志器，再激活。当前 Caddy 的域名日志映射按原始 Host 大小写匹配，单靠该映射会让大写 Host 落入默认日志；必须保留路由级绑定，并分别检查普通、大写和带端口的 Host。不要覆盖已有全局日志规则；存在冲突时先合并并重新验收。

已登记服务后，使用 `python3 deploy/tencent/apple_delivery_routes.py --source <实际部署的服务源码 SHA>` 配置路由。入口沿用 owner wrapper 和双锁，先确认活动 Caddy 与磁盘配置一致，再校验完整新配置；重载后读回活动配置，与已验证内容逐项比对。验证、安装、重载或状态写入失败时恢复原配置；失败的原子安装也会移除自身 pending 文件。当前共享主机没有其他命名日志器或全局日志覆盖，出现这类变更会拒绝自动修改，需先审阅兼容方案。

Caddy 的 [`file_server` 会自动隐藏输入配置文件](https://caddyserver.com/docs/caddyfile/directives/file_server#syntax)。预验证使用临时配置文件，因此仅将该处理器自动隐藏的临时输入路径映射为正式 Root 路径，再与活动配置比对；其他隐藏路径、处理器字段和日志规则不作放宽。

DNS 通过既有 Netlify 账号配置独立安装域名，指向当前主机。路由激活不等于公网 TLS 可用；仍须从外网检查证书和实际请求，验证失败请求不泄漏合成 URL，然后将完整代理配置加入新一份加密异地备份。

依据：[Caddy 全局日志规则](https://caddyserver.com/docs/caddyfile/options#log)、[请求日志规则](https://caddyserver.com/docs/caddyfile/directives/log)、[请求日志名称绑定](https://caddyserver.com/docs/caddyfile/directives/log_name) 和服务器当前使用的 [v2.11.4 日志实现](https://github.com/caddyserver/caddy/blob/v2.11.4/modules/caddyhttp/logging.go)。`apple_delivery_proxy_test.py` 使用真实 Caddy 和合成 URL 验证三种 Host 的失败请求不泄漏，并用其他站点作为日志仍可用的对照。测试绑定临时回环端口，不修改活动配置；本机缺少 Caddy 时明确跳过，不能据此声称代理已验收。

## 清理、回滚和完成条件

`cleanup` 先检查全部清单再删除过期授权；仅当没有有效授权且文件年龄超过两小时才移除 IPA。未知文件、符号链接或损坏清单使清理失败并保留文件。多个有效链接可以共用一个 IPA。

`apple-development` 已接入现有产物所有者机制。部署必须通过 `ops_artifacts.py` 的双锁 producer wrapper，登记当前镜像与匹配配置，保留上一份经过验证的回滚组合。`.operations/apple-delivery.json` 的 `image` 必须与当前 owner 登记匹配；私有数据目录固定为 `.apple-delivery/data`。计划内清理会使用登记镜像执行清理，失败写入运维结果并向上报错；dry-run 不执行删除。

以下都完成后才可报告远程安装可用：已登记部署及回滚、完整配置的加密异地备份、DNS/TLS、日志失败对照、真实 IPA 的 HTTPS 摘要读回、链接到期、计划任务自动清理，以及 iPhone 从外网安装/启动和双版数据隔离。签名材料不得进入安装服务。

当前服务源码为 `96a4d06`，已通过 owner wrapper 部署并登记。`install.tellyouwhat.cn` 的 DNS、TLS 和代理已接通，外网与服务器证书摘要一致；公开首页、健康检查和未知授权返回 404，附带禁止缓存/引用来源等响应头。服务继续为非 root、只读根文件系统/数据挂载及回环端口。

真实空仓库故障测试覆盖普通、大写及显式 :443 的 Host：均触发上游 502，服务随后恢复，日志窗口没有合成安装令牌。隔离实例还验证了未保护时会泄漏、其他站点仍有错误日志的对照。包含当前/保留旧版二进制与配置、实际 Caddy Root/片段和登记等二十个文件的加密备份，已完成认证、解密逐文件校验和 COS 三对象读回；这是手动运行，尚未证明定时任务或 SQL 恢复。

保留的 `0e9ce2a` 是服务级旧版，其原代理模板缺少修复后的日志绑定；不得恢复该旧代理。当前代理入口会拒绝缺少无条件绑定的模板。完整跨版本回滚仍待验收。

首个实际 IPA 已发布：生日 1.0 (2)、源码 `d29ed18`。该源码的应用/环境检查 54 项通过、1 项真实服务跳过，领域包 27 项通过；主包和 Widget 原生签名再次核验通过。公网 Manifest/完整 IPA 摘要、HEAD/Range、三种 Host 和一分钟真实到期 410 已通过，24 小时受限链接已私下交付用户。发布包装器回收旧授权 1 项，属于手动触发，不能代替定时清理。手机安装、真实测试 API、完整回滚及其他 App 的交付仍待验收，见 [生日 OTA 证据](evidence/birthday-ota-delivery-2026-10-16.json)。详见 [HTTPS 与实际故障证据](evidence/apple-development-https-2026-10-16.json)；早期回环部署记录保留在 [部署证据](evidence/apple-development-delivery-deployment-2026-10-16.json)。

健康 1.0.2 (3) 已通过相同受限入口发布：源码 `75c96ac4`，Xcode 27.1 的本机完整 unit 流程 1,623 项零失败/跳过、双平台 Release 编译通过，随后同工具链导出并核对七个原生签名目标。约 149 MB 的实际 HTTPS 下载摘要、HEAD/Range 及等价 Host 已通过，用户安装链接私有保存。手机及真实云端旅程仍未验收；见 [健康 OTA 证据](evidence/health-ota-delivery-2026-10-16.json)。
