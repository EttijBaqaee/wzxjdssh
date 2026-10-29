# wzxjdssh

以 Docker 部署的 SSH 诱捕服务，支持真实 SSHv2 握手、延迟拒绝认证，以及可选的慢速横幅模式。

默认完成版本交换、密钥交换、服务器签名和加密传输，再让认证请求等待后失败。没有认证成功的入口，不提供 Shell、SFTP 或端口转发。

当前开发版本：**2.0.0**

## 工作原理

默认 `Mode ssh`：

1. 接收 TCP 连接并发送 SSH 服务端版本标识。
2. 使用持久化主机密钥完成真实 SSHv2 密钥交换和加密传输。
3. 接受密码、公钥探测或 keyboard-interactive 认证流程，按 `Delay` 等待后拒绝认证。
4. 达到认证次数、超时或资源上限时关闭连接。

默认每次认证延迟 **10 秒**，单连接最多 **6 次认证失败**。客户端自己的超时或主动断开也会结束连接。

可选 `Mode banner` 在版本交换前持续、缓慢地发送随机可打印字符和 CRLF；首行同样等待 `Delay`。这种模式不完成真实 SSH 握手。

## 功能

- 支持密码、公钥探测、keyboard-interactive，所有认证最终拒绝。
- 保存 Ed25519、ECDSA P-256、RSA 3072 主机密钥，保留数据卷即可在重建后沿用。
- 通过编排环境参数修改宿主机入口端口和 SSH 版本标识。
- 支持总连接数、每 IP 并发、每 IP 速率、握手并发及多种超时限制。
- 严格配置校验；配置重载失败保留旧监听和旧配置。
- 内置 SQLite，记录连接状态、来源、时间、时长、客户端标识和认证尝试，默认保留 90 天。
- 记录用户名、密码、keyboard-interactive 回答，以及客户端提交的公钥正文和 SHA256 指纹；SSH 不传输客户端私钥，公钥探测也不证明持有对应私钥。
- 内置中文 Web UI：统计概览、最近 30 天趋势、来源/用户名/认证方式排行、连接筛选和详情。
- Web UI 管理员密码通过编排设置，支持路径前缀及 HTTP/HTTPS 反代。
- 支持 JSON 统计、日志级别和优雅退出；认证内容保存在数据库中，不写入标准输出日志。
- 正式镜像基于 scratch，以非 root 用户运行，内置健康检查。

## Docker 部署

配套文件：[compose.release.yaml](compose.release.yaml)、[环境参数示例](.env.example)、[完整编排文档](COMPOSE.zh-CN.md)。

1. 先在 1Panel 中构建本仓库的 `Dockerfile`，标签设为 `wzxjdssh:2.0.0`；或导入自行保存的同标签镜像。Dockerfile 的最终阶段为正式运行镜像，测试工具不会进入最终镜像。
2. 在“容器 → 编排”创建编排，粘贴 `compose.release.yaml`。
3. 在编排环境参数区设置至少 12 字节的 `WZXJDSSH_ADMIN_PASSWORD`，其他参数可使用默认值。
4. 启动后确认容器健康状态。从指定端口连接时，应能完成 SSH 握手，认证随后被延迟拒绝。

编排使用 `pull_policy: never`，直接使用已构建或导入的本地镜像。

### 常用编排参数

| 参数 | 默认值 | 作用 |
| --- | --- | --- |
| `WZXJDSSH_PORT` | `2222` | 宿主机入口端口；容器内部仍监听 2222 |
| `WZXJDSSH_SERVER_VERSION` | `SSH-2.0-OpenSSH_10.0p2 Debian-7+deb13u4` | 对外发送的 SSH 版本标识，未设置或留空时使用此默认值 |
| `WZXJDSSH_WEB_PORT` | `2223` | Web UI 宿主机端口 |
| `WZXJDSSH_WEB_BIND` | `127.0.0.1` | Web 绑定地址；直接远程访问可设 `0.0.0.0` |
| `WZXJDSSH_ADMIN_USERNAME` | `admin` | 管理员用户名 |
| `WZXJDSSH_ADMIN_PASSWORD` | 必填 | 管理员密码，至少 12 字节 |
| `WZXJDSSH_WEB_BASE_PATH` | 空，使用 `/` | 路径前缀，例如 `/sshpanel` |
| `WZXJDSSH_WEB_SECURE_COOKIE` | `false` | 通过 HTTPS 访问时设置 `true` |
| `WZXJDSSH_RETENTION_DAYS` | `90` | 历史记录保留天数，范围 1–36500 |

例如，发布到宿主机 22，并自定义版本标识：

```dotenv
WZXJDSSH_PORT=22
WZXJDSSH_SERVER_VERSION=SSH-2.0-OpenSSH_9.9
WZXJDSSH_ADMIN_PASSWORD=replace-with-your-own-long-password
```

仅修改端口、保留 Debian 标识：

```dotenv
WZXJDSSH_PORT=22
WZXJDSSH_SERVER_VERSION=
WZXJDSSH_ADMIN_PASSWORD=replace-with-your-own-long-password
```

标识必须是完整的 `SSH-2.0-...` 字符串，可以带发行版注释；不接受换行、控制字符或非 ASCII 字符。这里只改变版本标识，不改变底层 SSH 实现。

## 默认配置

镜像配置路径为 `/etc/wzxjdssh/config`，模板见 [configs/wzxjdssh.conf](configs/wzxjdssh.conf)。时间单位均为毫秒。

| 配置 | 默认值 | 说明 |
| --- | --- | --- |
| `Port` | 2222 | 容器内监听端口 |
| `BindAddress` | 未指定 | 监听通配地址，可设 IP 字面量 |
| `BindFamily` | 0 | 自动选择地址族；4 为 IPv4，6 为 IPv6，双栈行为取决于系统 |
| `Mode` | ssh | ssh / banner |
| `Delay` | 10000 | 认证失败前等待，或 banner 模式行间隔，首行也等待 |
| `MaxLineLength` | 32 | 仅用于 banner 模式，范围 3–255，包含 CRLF |
| `MaxClients` | 4096 | 总接入上限，范围 1–2147483647；实际容量受系统资源限制 |
| `MaxClientsPerIP` | 64 | 每 IP 并发上限；0 关闭此限制 |
| `ConnectionsPerMinute` | 120 | 每 IP 令牌桶补充速率及突发容量；0 关闭 |
| `MaxPendingHandshakes` | 128 | 同时进行的 SSH 握手上限 |
| `MaxAuthTries` | 6 | 单连接认证失败次数上限 |
| `HandshakeTimeout` | 20000 | 版本交换和密钥交换超时 |
| `IdleTimeout` | 120000 | 认证阶段客户端空闲超时；服务端主动等待不计入 |
| `WriteTimeout` | 5000 | 一次完整写入的超时 |
| `MaxConnectionTime` | 3600000 | 单连接总寿命上限 |
| `HostKeyDirectory` | /data | 主机密钥目录，启动后不能热改 |
| `ServerVersion` | SSH-2.0-OpenSSH_10.0p2 Debian-7+deb13u4 | 配置文件中的默认 SSH 标识 |
| `LogLevel` | 0 | 0 重要事件，1 连接与认证事件，2 调试 |

配置接受空格、Tab 和 `#` 注释。未知键、重复键、非法数值和打不开的显式配置文件均报错。命令行选项覆盖相应配置；`-v` 开启连接日志，`-vv` 开启调试日志。

**标识优先级：**非空 `WZXJDSSH_SERVER_VERSION` 环境变量覆盖配置文件中的 `ServerVersion`。配套编排始终传入该值，留空时展开为 Debian 默认值。若通过挂载配置文件定制了 `ServerVersion`，应将编排参数也设置为同样的值，或移除服务 `environment` 中的这个变量。

## 数据与运维

`hostkeys:/data` 保存主机密钥，`records:/var/lib/wzxjdssh` 保存 SQLite 数据库及其 WAL 文件。升级时编辑原编排、保留同一个项目名和两个数据卷；新建编排通常会生成另一组卷。自定义目录挂载需允许 UID/GID `65532:65532` 读写。

数据库保存对方实际提交的认证内容，请限制管理入口和备份的访问权限。只部署一个实例读写同一数据库目录。备份时先停止编排服务，再备份整个 `records` 卷；运行中备份应使用 SQLite Backup API，不能只复制主数据库而遗漏 WAL。

程序启动及每小时清理过期记录。认证按提交时间过期，已结束连接按开始时间过期；仍在线的连接在结束后清理。删除记录会供 SQLite 重用空间，不保证立即缩小文件或擦除外部快照。Web 总览覆盖当前保留数据，趋势显示最近 30 天，日统计按 UTC，详情时间按浏览器本地时区展示。

Web 管理会话有效期 8 小时，退出或容器重启后失效。反代保留路径前缀的 Nginx 示例和升级步骤见 [编排文档](COMPOSE.zh-CN.md)。

| 接口 | 行为 |
| --- | --- |
| `SIGHUP` | 校验并重载配置；更换监听失败时保留旧配置与连接 |
| `SIGUSR1` | 输出 JSON 统计 |
| `SIGTERM` / `SIGINT` | 关闭监听、取消等待并退出 |
| `-check` | 校验配置与版本标识环境参数 |
| `-health` | Web 启用时检查就绪接口与数据库，不生成 SSH 访问记录；关闭 Web 时检查 SSH 监听 |
| `-V` | 输出程序版本 |

已接入连接保留自己的协议模式、超时和认证上限；`Delay` 与 banner 行长可动态更新。通过编排修改端口或环境变量时应重新部署容器。

## 许可证

本项目采用 [MIT License](LICENSE)。第三方依赖遵循各自许可证，见 [第三方许可声明](THIRD-PARTY-NOTICES.txt)。
