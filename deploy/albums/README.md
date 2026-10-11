# 相册独立测试环境

`cmd/albumgateway` 装配真实 MySQL 账户、加密 Apple refresh grant、单次登录挑战、可撤销会话、Redis 限流、上传/分片/图库/日常副本接口。`cmd/albumworker` 执行归档验证和媒体处理。新登录账户容量为零；订阅和内部测试权益尚未装配，不能把登录成功视为允许云备份。

`compose.albums-test.yaml` 是独立 Compose 项目：固定 `albums_test` 数据库、专用 MySQL 卷、专用 Redis 和网络、仅 `127.0.0.1:18090` 的 API 入口。迁移全部在这个新库运行。启动服务前检查身份表、上传/分片/派生/实况表与 COS 版本控制；运行中 `/readyz` 检查数据库、Redis 和退出状态。健康检查不等于 worker、Apple、转码或原件恢复通过。

当前 `tellyouwhat-albums-1251995834` 专用于相册开发联调；测试原件/派生前缀全部处于这个测试桶内。正式服务启用前配置独立正式桶与身份，正式数据不得接入此测试栈；不能通过复用这个数据库或桶把测试环境直接当作正式服务。

## 凭据与准备

1. 服务器 `/opt/tellyouwhat/albums-test/` 专用目录，所有者为部署用户，权限 `0700`。核对现有服务内存/磁盘余量、18090 端口和 Docker 子网，默认子网 `172.29.90.0/24`；实际 host gateway 必须与 `ALBUM_TEST_PROXY_CIDR` 一致。
2. `test.env.example` 复制为私有 `.env.compose-albums-test`，只供 Compose 插值；`runtime.env.example` 复制为独立 `.env.albums-test`，只供应用 `env_file`。两个文件均 `0600`；前者的 root 数据库密码不能传给 API/worker。密码分别生成，数据库/Redis 密码使用随机十六进制，避免 DSN/URL/Compose 转义问题。
3. Apple Sign in `.p8` 必须实际授权团队 `T6DD96HY2B` 与 App ID `cn.tellyouwhat.albums`。加密密钥、登录 IP HMAC 密钥各生成独立随机 32 字节，再以标准 Base64 保存到对应文件。应用拒绝同一密钥、非法格式、公共可读或可写文件。须保留加密密钥，换掉它会导致既有 Apple grant 无法解密。
4. `secrets/` 在宿主机设为 `root:65532 0750`，三个文件设为 `root:65532 0440`，让容器 UID/GID 65532 可读。只把该目录挂给 API；worker 不接收 Apple 私钥和账户加密文件。密钥不进入 Git、App、日志或聊天。
5. 腾讯 CAM 新身份分别命名 `albums-test-upload`、`albums-test-backend`，仅绑定对应策略文件；保留独立 Secret ID/Key。策略草案尚未应用，需创建访问权限前的确认及实际授权验收。上传身份只写暂存；后端身份读写相册范围、管理分片并查询/提交媒体任务。没有对象永久删除、ACL/桶管理、其他桶或 CAM 管理权限。若身份另有策略，必须核对总权限。

权限格式及操作依据：[COS 分片上传](https://cloud.tencent.com/document/product/436/65644)、[COS 复制](https://intl.cloud.tencent.com/zh/document/product/436/78078)、[CI 授权粒度](https://cloud.tencent.com/document/product/460/41741)。CAM 文件通过静态范围检查也不证明真实服务会允许/拒绝；真实账号还需校验分片复制、固定版本读取、越界写入和 CI 服务角色范围。派生回收保持关闭，所需版本列举/永久删除权限后续单独配置和验收。

## 构建与接入

使用已评审提交的干净源码 checkout，将完整 Git SHA 写入 `ALBUM_TEST_IMAGE_TAG`。PR 容器构建只验证、不发布镜像；在独立测试目录按这个提交构建三份镜像，不能把移动的 `main` 标签当成相册 PR 的产物。

```sh
docker compose --env-file /opt/tellyouwhat/albums-test/.env.compose-albums-test \
  -f compose.albums-test.yaml config --quiet
docker compose --env-file /opt/tellyouwhat/albums-test/.env.compose-albums-test \
  -f compose.albums-test.yaml build albumgateway albumworker migrate
docker compose --env-file /opt/tellyouwhat/albums-test/.env.compose-albums-test \
  -f compose.albums-test.yaml up -d
curl --fail --silent --show-error -H 'Host: api.albums.tellyouwhat.cn' \
  http://127.0.0.1:18090/readyz
```

仅渲染 `Caddyfile.external` 的公开域名和端口并导入现有 Caddyfile；完整配置验证后 reload。核对相册公网 HTTPS、未知路径 404、其他既有域名健康。凭据环境文件不得交给 Caddy。测试失败可单独 `docker compose … stop albumgateway albumworker`；保留测试数据库和对象用于恢复，普通停止不用 `down -v`。

## 接入通过条件

- Apple App ID/Sign in Key 与真实授权匹配，退出、重启及另一进程的会话撤销正确；伪造转发头不能绕过限流。
- 以程序生成媒体完成真实原件上传、精确版本归档校验、图片/视频处理、完整实况对上传/读取/系统保存；记录源码 SHA、对象版本、大小/哈希、任务和计费证据。处理开关按实测结果逐项启用。
- 验证上传身份无法读取或写归档、操作其他桶；后端无法操作其他桶、修改权限或永久删除。不得用扩大到 `*` 权限解决 403。
- 为真实测试账户装配受审计的测试权益，或完成订阅权益验证后才授权容量。当前没有开放临时容量接口，也没有客户端指定账户/额度入口。
- 原件安全释放、实况/视频画质、真机发热/后台/微信/短信、费用和清理均独立验收。当前仅有受控服务装配验证，尚未实际部署此栈或启动付费处理。
