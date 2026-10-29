# wzxjdssh 2.0.0：1Panel 编排使用文档

配套文件：[compose.release.yaml](compose.release.yaml)。镜像为 `wzxjdssh:2.0.0`，支持 Linux amd64。默认公开标识为 `SSH-2.0-OpenSSH_10.0p2 Debian-7+deb13u4`，可以通过编排环境参数修改。

先在 1Panel 构建或导入本地镜像，然后创建编排、粘贴配套 YAML。在编排的环境参数区至少设置 `WZXJDSSH_ADMIN_PASSWORD`，密码长度至少 12 字节。不要将实际密码提交到 Git。

## 端口映射

默认配置采用编排环境参数，未设置时发布到宿主机 `2222`：

```yaml
ports:
  - "${WZXJDSSH_PORT:-2222}:2222"
```

左侧是宿主机入口，右侧 `2222` 是容器内监听端口。默认绑定宿主机所有可用地址；实际网络可达性还取决于宿主机防火墙、云安全组和路由。

在 1Panel **编排的环境参数 / .env** 编辑区填写以下内容，即可将入口改成 `22`：

```dotenv
WZXJDSSH_PORT=22
```

要恢复默认入口，删除此参数或设置 `WZXJDSSH_PORT=2222`；也可填其他有效端口。修改后重新部署编排应用映射。这个参数用于 Compose 展开端口映射，应写在编排的环境参数中，单独放进容器服务的 `environment:` 不会改变宿主机发布端口。

可直接复制 [.env.example](.env.example) 的内容到 1Panel 的编排环境参数区。

只供本机访问可把映射改为 `"127.0.0.1:${WZXJDSSH_PORT:-2222}:2222"`。更改容器内 `Port` 配置时，右侧端口也必须同步修改。容器本身以非 root 用户监听 `2222`，宿主机端口映射由 Docker 完成。

## 编排参数

### Web UI 与记录保存

| 环境参数 | 默认值 | 说明 |
| --- | --- | --- |
| `WZXJDSSH_ADMIN_USERNAME` | `admin` | 管理员用户名 |
| `WZXJDSSH_ADMIN_PASSWORD` | 必填 | 至少 12 字节，无默认密码 |
| `WZXJDSSH_WEB_BIND` | `127.0.0.1` | Web 宿主机绑定地址 |
| `WZXJDSSH_WEB_PORT` | `2223` | Web 宿主机端口，容器内固定 2223 |
| `WZXJDSSH_WEB_BASE_PATH` | 空 | 前缀如 `/sshpanel`；支持字母、数字、`-`、`_` 组成的多级路径 |
| `WZXJDSSH_WEB_SECURE_COOKIE` | `false` | HTTPS 访问时设置 `true` |
| `WZXJDSSH_RETENTION_DAYS` | `90` | 保留天数，范围 1–36500 |

直接通过局域网访问的参数示例：

```dotenv
WZXJDSSH_ADMIN_PASSWORD=replace-with-your-own-long-password
WZXJDSSH_WEB_BIND=0.0.0.0
WZXJDSSH_WEB_PORT=2223
WZXJDSSH_WEB_BASE_PATH=/sshpanel
WZXJDSSH_RETENTION_DAYS=90
```

访问 `http://服务器地址:2223/sshpanel/`，使用 `admin` 和编排密码登录。不填路径参数则访问 `/`。闲置默认 1 小时自动退出，可在“管理设置”修改为 1–1440 分钟，保存后立即生效。此设置保存在 SQLite 中，保留 `records` 卷即可在重启、升级后沿用。自动更新不延长会话；退出、容器重启或修改密码重新部署后，原会话失效。

总览展示保留期内的总连接、认证尝试、独立来源 IP、今日连接及接入拒绝数，并显示当前在线连接；趋势展示最近 30 天的连接和认证次数，另有来源 IP、用户名、认证方式排行。日统计使用 UTC。连接记录支持按 IP、用户名、状态筛选，详情显示密码/交互回答或公钥及指纹。SSH 不传输客户端私钥，公钥探测不能证明私钥持有关系。

总览的“拖延中 IP”按当前正在延迟响应的来源 IP 去重；同一 IP 的多条连接单独显示在“拖延中连接”。在线连接榜按累计服务端延迟时长降序排列，包含尚未完成的等待，不把握手或客户端思考时间计入拖延。单 IP 并发榜按当前在线连接数排序，包含握手、等待认证和延迟响应阶段；两榜最多显示 10 项，断开连接后在下次刷新移出。开启自动更新时每 10 秒更新一次。

当前生效的拖延策略在“管理设置”展示；修改服务配置并成功重载或重启后，重新进入管理设置或点击刷新即可读取新值。

### 反代到 80 / 443

例如希望通过 `https://example.com/sshpanel/` 访问，在编排环境参数设置：

```dotenv
WZXJDSSH_ADMIN_PASSWORD=replace-with-your-own-long-password
WZXJDSSH_WEB_BIND=127.0.0.1
WZXJDSSH_WEB_BASE_PATH=/sshpanel
WZXJDSSH_WEB_SECURE_COOKIE=true
```

反代位于宿主机时，Nginx location 示例：

```nginx
location = /sshpanel {
    return 308 /sshpanel/;
}
location /sshpanel/ {
    proxy_pass http://127.0.0.1:2223;
    proxy_set_header Host $http_host;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_http_version 1.1;
}
```

`proxy_pass` 末尾不加 `/`，必须保留 `/sshpanel/` 前缀转发；前端资源、API 与 Cookie 都使用该前缀。HTTP 80 也可采用同样配置，纯 HTTP 时保持 `WZXJDSSH_WEB_SECURE_COOKIE=false`。需要证书时在 1Panel 网站中配置 HTTPS。

若 1Panel 的 OpenResty 位于容器中，它的 `127.0.0.1` 指向自身。此时可让两服务加入同一个 Docker 网络，反代至 `http://wzxjdssh:2223`；或使用反代容器可达的宿主机地址，同时调整 Web 绑定地址。使用共享网络时保留路径前缀及 Host 转发配置。应用不依赖 `X-Forwarded-Prefix`，也不通过转发头自动开启 Secure Cookie。

登录限速按直接连接 Web 的来源地址计算：5 分钟内最多 10 次尝试。经过反代时可能共用同一限速桶；应用不信任外部传入的 IP 头。

### SSH 版本标识

在 1Panel 编排环境参数区填写：

```dotenv
WZXJDSSH_SERVER_VERSION=SSH-2.0-OpenSSH_9.9
```

留空或不设置这个参数，就使用 `SSH-2.0-OpenSSH_10.0p2 Debian-7+deb13u4`。端口和标识可以同时填写：

```dotenv
WZXJDSSH_PORT=22
WZXJDSSH_SERVER_VERSION=
```

编排使用以下表达式传入标识，`:-` 对“未设置”和“空值”都使用默认值：

```yaml
environment:
  WZXJDSSH_SERVER_VERSION: "${WZXJDSSH_SERVER_VERSION:-SSH-2.0-OpenSSH_10.0p2 Debian-7+deb13u4}"
```

必须填写完整的 `SSH-2.0-...` 标识，允许空格后的发行版注释，不允许换行、控制字符或非 ASCII 字符。修改后重新部署容器。此参数只影响 SSH 服务端版本标识，不改变握手算法、认证策略，也不改变 `Mode banner` 的随机文本内容。

### 资源与运行设置

| 字段 | 当前值 | 用途 |
| --- | --- | --- |
| image | wzxjdssh:2.0.0 | 指定本地镜像版本 |
| pull_policy | never | 使用本地镜像，不从远端拉取 |
| container_name | wzxjdssh | 固定容器名；同一宿主机部署多份时需分别命名或移除此字段 |
| restart | unless-stopped | 自动恢复容器；手动停止后保持停止 |
| volumes | hostkeys:/data、records:/var/lib/wzxjdssh | 保存主机密钥与 SQLite 记录 |
| read_only | true | 根文件系统只读；密钥数据卷仍可写 |
| cap_drop | ALL | 移除 Linux capabilities |
| security_opt | no-new-privileges:true | 禁止进程获得额外权限 |
| mem_limit | 512m | 内存上限，超过时可能被终止；按连接量调整 |
| cpus | 1 | CPU 配额上限；真实密钥交换会消耗 CPU |
| pids_limit | 256 | 限制容器内任务数量；不是连接数限制 |
| ulimits.nofile | 16384 | 文件描述符软、硬上限 |
| logging | json-file，10m × 3 | 限制 Docker 日志文件大小及保留数量 |

镜像内用户为 UID/GID `65532:65532`，健康检查调用内置 `/wzxjdssh -health`。正式镜像基于 scratch，没有 shell、包管理器或编译器，使用面板的容器日志查看运行情况。

`MaxClients` 默认是 4096，属于接入限制，并非这套资源配额已验证的容量。需要结合实际握手速率、内存和 CPU 调整。

## 密钥卷与升级

`hostkeys` 是 Docker 命名卷；默认实际卷名包含编排项目名。因此，升级时应编辑原有编排，保留同一个编排名和卷声明。另建编排通常会创建新卷，从而生成不同主机密钥。

升级步骤：先在 1Panel 导入新镜像，再修改原编排的 `image` 标签并重新部署。保留 `hostkeys:/data` 即可沿用原密钥。回滚同样通过修改镜像标签完成。删除数据卷会丢失密钥；备份和恢复时保留文件权限，并限制备份的读取权限。

从 1.x 升级到 2.0：替换编排内容时保留原编排名及 `hostkeys` 卷，新增 `records:/var/lib/wzxjdssh` 和顶层 `records` 卷声明，设置管理员密码。1.x 没有记录历史凭据，升级后才开始积累记录。此后的升级同时保留两个卷。

数据库默认路径为 `/var/lib/wzxjdssh/records.sqlite`，使用 WAL 模式。备份请在 1Panel 停止服务后备份整个 `records` 卷，或通过具备 SQLite Backup API 的工具在线备份；运行中只复制主数据库文件可能遗漏尚在 WAL 中的数据。不要把同一个数据库目录挂给多个运行实例。

清理在启动时和每小时进行。认证按提交时间清理，已结束连接按开始时间清理；仍在线连接在结束后清理。删除记录后的空闲空间可重用，不保证数据库立即缩小，也不会同步清除已有备份或快照。

若改用宿主机目录挂载 `/data`，该目录需允许容器 UID/GID `65532:65532` 读写。命名卷由 Docker 使用镜像内目录的属主和权限初始化。

## 自定义配置

默认使用镜像内 `/etc/wzxjdssh/config`。完整字段表见 [README](README.md)。修改设置时，可在编排中额外挂载配置目录，并覆盖 `WZXJDSSH_CONFIG`。

以下为配置挂载示例。先将仓库内的 `configs/wzxjdssh.conf` 放到宿主机自己的配置目录，示例使用 `/opt/wzxjdssh/configs`：

```yaml
environment:
  WZXJDSSH_SERVER_VERSION: "${WZXJDSSH_SERVER_VERSION:-SSH-2.0-OpenSSH_10.0p2 Debian-7+deb13u4}"
  WZXJDSSH_CONFIG: /etc/wzxjdssh-custom/wzxjdssh.conf
volumes:
  - hostkeys:/data
  - /opt/wzxjdssh/configs:/etc/wzxjdssh-custom:ro
```

此处 `volumes` 和 `environment` 均与原服务中的同名键合并，不要重复声明。配置文件名为 `wzxjdssh.conf`，时间单位为毫秒，例如 `Delay 10000` 表示每次认证等待 10 秒。

非空 `WZXJDSSH_SERVER_VERSION` 优先于文件中的 `ServerVersion`。使用配套编排时，留空的编排参数会展开为 Debian 默认值；要使用自定义配置文件里的其他标识，需要同步设置编排参数，或移除服务 `environment` 中的 `WZXJDSSH_SERVER_VERSION`。

程序默认参数为：`Port 2222`、`Delay 10000`、`MaxLineLength 32`（允许 3–255，含 CRLF）、`MaxClients 4096`（允许 1–2147483647）、`LogLevel 0`、`BindFamily 0`。自动地址族在系统支持时使用 IPv6 双栈覆盖 IPv4，实际行为取决于系统。`Mode banner` 的首行也等待 Delay；`Mode ssh` 先完成真实 SSH 握手，再按 Delay 延迟认证失败响应。

客户端可以主动中止连接。固定的连接或认证总超时不会被服务端延迟、认证提示或 KeepAlive 延长，例如 ssh2 1.16.0 的 `readyTimeout` 默认 20 秒，认证成功前持续计时。

若对端在认证延迟期间主动断开，服务记录“认证中断（可能超时）”，按来源 IP 与完整客户端 SSH 标识建立 1 小时冷却标记。服务端无法得知客户端本地的确切退出原因，因此手动取消也可能触发此标记。冷却期间再次匹配并提交认证时，返回认证成功后立即关闭连接，不开放会话、命令或转发通道；密码、交互回答或公钥继续记录，公钥认证仍需客户端完成协议要求的签名。

冷却到期自动恢复正常拖延，期间重连不延长到期时间。标记存于 SQLite，保留 `records` 卷即可跨重启沿用；连接详情显示标记、到期时间与仿真处理结果。历史标签按连接记录保留，过期的活动标记会被清理。

配置保存后，通过 1Panel 重启容器应用修改。若面板提供发送信号的入口，也可向服务进程发送 `SIGHUP` 进行事务重载；新配置非法或新端口无法绑定时保留旧配置。变更 Docker 端口映射属于编排变更，需要重新部署容器。单文件 bind mount 配合原子替换可能仍指向旧文件，因此示例挂载整个配置目录。

默认 `LogLevel 0` 只输出重要事件。需要连接日志时将配置设为 `LogLevel 1`，或给服务增加 `command: ["-v"]`；调试使用 `LogLevel 2` 或 `command: ["-vv"]`。
