# wzxjdssh 2.0.4： 编排使用文档

配套文件：[compose.release.yaml](compose.release.yaml)。镜像为 `ghcr.io/ettijbaqaee/wzxjdssh:2.0.4`，支持 Linux amd64；另提供 `latest` 标签。默认公开标识为 `SSH-2.0-OpenSSH_10.0p2 Debian-7+deb13u4`，可以通过编排环境参数修改。

创建编排、粘贴配套 YAML，首次部署会从 GHCR 拉取镜像。在编排的环境参数区至少设置 `WZXJDSSH_ADMIN_PASSWORD`，密码长度至少 12 字节。不要将实际密码提交到 Git。

离线环境可下载 GitHub Release 中的镜像归档，在 1Panel 导入，再将 `image` 改为 `wzxjdssh:2.0.4`、`pull_policy` 改为 `never`。

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
| `WZXJDSSH_ADMIN_USERNAME` | `admin` | 管理员用户名，未填写或留空时使用 admin |
| `WZXJDSSH_ADMIN_PASSWORD` | 必填 | 至少 12 字节，无默认密码 |
| `WZXJDSSH_WEB_BIND` | `127.0.0.1` | Web 宿主机绑定地址 |
| `WZXJDSSH_WEB_PORT` | `2223` | Web 宿主机端口，容器内固定 2223 |
| `WZXJDSSH_WEB_BASE_PATH` | 空 | 前缀如 `/sshpanel`；支持字母、数字、`-`、`_` 组成的多级路径 |
| `WZXJDSSH_WEB_SECURE_COOKIE` | `false` | HTTPS 访问时设置 `true` |
| `WZXJDSSH_RETENTION_DAYS` | `90` | 保留天数，范围 1–36500 |
| `WZXJDSSH_DATABASE_MAX_MB` | `0`，不启用 | 主库及 WAL/SHM 合计容量上限，单位 MiB；1024 为 1 GiB |
| `WZXJDSSH_MAX_AUTH_TRIES` | `0`，不限次数 | 单连接认证上限，慢速阶段不限总持续时间 |
| `WZXJDSSH_CONNECTIONS_PER_MINUTE` | `2048` | 每 IP 令牌桶每分钟补充速率及突发容量，0–1000000；0 关闭限制 |
| `WZXJDSSH_MAX_CLIENTS` | `4096` | 总连接上限，1–2147483647 |
| `WZXJDSSH_MAX_CLIENTS_PER_IP` | `64` | 每 IP 并发，0–100000；0 关闭限制 |
| `WZXJDSSH_MAX_PENDING_HANDSHAKES` | `128` | 同时握手上限，1–100000 |
| `WZXJDSSH_DELAY_MS` | `10000` | 每次主动拖延，1–86400000 毫秒 |
| `WZXJDSSH_DELAY_JITTER_PERCENT` | `10` | 普通认证、横幅、并发慢速响应的随机扰动，0–50% |
| `WZXJDSSH_PARALLEL_WINDOW_MS` | `60000` | 同 IP 重叠连接判定窗口，1–3600000 毫秒 |
| `WZXJDSSH_PARALLEL_CONNECTIONS` | `2` | 触发并发慢速策略的重叠连接数，2–100000 |
| `WZXJDSSH_PROBE_FAILURE_THRESHOLD` | `3` | 同 IP 连续握手未完成阈值，1–100000 |
| `WZXJDSSH_PROBE_COOLDOWN_MS` | `900000` | 探针静默冷却，1–86400000 毫秒；期间重连不续期 |
| `WZXJDSSH_SINGLE_AUTH_RETRY_THRESHOLD` | `3` | 连续单次密码拒绝后退出的阈值；0 禁用，范围 0–100000 |
| `WZXJDSSH_SINGLE_AUTH_RETRY_WINDOW_MS` | `900000` | 连续计数窗口，1–86400000 毫秒；按 IP 和完整 SSH 标识区分 |
| `WZXJDSSH_SINGLE_AUTH_RETRY_COOLDOWN_MS` | `3600000` | 触发后的冷却，1–604800000 毫秒；重连不续期 |
| `WZXJDSSH_PARALLEL_COOLDOWN_MS` | `3600000` | 最后一条连接断开后闲置冷却，1–604800000 毫秒；重连重新计时 |
| `WZXJDSSH_HANDSHAKE_TIMEOUT_MS` | `20000` | 握手超时，1–3600000 毫秒 |
| `WZXJDSSH_IDLE_TIMEOUT_MS` | `120000` | 认证空闲超时，1–86400000 毫秒；主动拖延不计入 |
| `WZXJDSSH_WRITE_TIMEOUT_MS` | `5000` | 单次完整写入超时，1–60000 毫秒 |
| `WZXJDSSH_MAX_CONNECTION_TIME_MS` | `0` | 慢速阶段前寿命，0–604800000 毫秒；慢速阶段仍不限时 |
| `WZXJDSSH_MODE` | `ssh` | ssh / banner |
| `WZXJDSSH_MAX_LINE_LENGTH` | `32` | banner 模式行长，3–255 字节，含 CRLF |
| `WZXJDSSH_LOG_LEVEL` | `0` | 0 重要事件，1 连接日志，2 调试 |

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

### 慢速连接持续时间与满载回收

自 2.0.3 起，慢速横幅、SSH 延迟认证及仿真网络会话不限总持续时间，管理设置中的“连接最长持续”显示“不限时”。不需要新增编排参数；旧挂载配置中仍有 `MaxConnectionTime 3600000` 时，慢速阶段也会取消总寿命计时。默认配置现为 `MaxConnectionTime 0`，非零值仅约束进入慢速阶段之前的连接。

新的合格连接到达且总连接数 `MaxClients` 或同 IP 并发 `MaxClientsPerIP` 已满时，按需回收保持最久的慢速连接，等待释放后接纳新连接；单 IP 满载只回收同 IP。没有新连接时，即使正好满载也不自动清退。握手并发限制、接入速率限制拒绝的新连接不会挤掉旧连接；没有足够可回收的慢速连接时，拒绝新连接。记录中的“容量回收”与客户端认证超时分开，不生成冷却标记。

客户端主动退出、网络读写错误、服务停止、握手超时及客户端真正空闲仍按原有逻辑处理；主动拖延期间不计客户端空闲。自定义的非零 `WZXJDSSH_MAX_AUTH_TRIES` 也继续生效。

### 数据库容量保护

编排环境参数留空或设为 0 时，容量清理不启用。要在超过 1 GiB 后自动清理，在编排的环境参数区填写：

```dotenv
WZXJDSSH_DATABASE_MAX_MB=1024
```

配套 YAML 已将此参数传入容器；自行编写编排时，在服务 `environment:` 添加：

```yaml
WZXJDSSH_DATABASE_MAX_MB: "${WZXJDSSH_DATABASE_MAX_MB:-0}"
```

超过上限后，从最旧连接开始清理，已结束连接与其认证记录一并删除；在线连接保留元数据，清理其旧认证内容。容量清理会合并 WAL 并回收 SQLite 文件空间，目标约为上限的 90%。写入后检查并每秒补查，写入和回收期间可能短暂超限；旧库完整 VACUUM 可能需要额外临时磁盘空间。这个参数控制数据库，不包括日志、备份和主机密钥。保留天数与容量限制同时生效，先达到哪个条件就按哪个条件清理。

### IP 汇总与并发策略

连接记录主表以 IP 为单位，显示连接次数、当前在线、最近连接、认证次数和最长在线时长；点击“查看连接”展开来源端口与认证详情。筛选后只汇总符合条件的连接。两层分页均支持“最新一页”“最旧一页”。

密码榜统计密码认证和交互认证的单个密码回答。单 IP 拖延榜按该 IP 单次连接的最长实际拖延排序，同时显示累计拖延；包含在线和已结束连接，旧版没有记录的时长不做推算。

默认同 IP 在 60 秒内出现至少两条重叠连接时，全部连接使用慢速策略，后续认证延迟拒绝；清除该 IP 的旧冷却标记，这组连接中途断开也不产生认证超时标记。已有仿真会话改为基准间隔叠加随机扰动的响应。标记 IP 仍有连接时保持该策略；最后一条连接断开后，默认闲置 1 小时冷却，期间重连重新计时。冷却与并发判定窗口分开，窗口只用于首次识别。可在编排修改 `WZXJDSSH_PARALLEL_WINDOW_MS`、`WZXJDSSH_PARALLEL_CONNECTIONS` 和 `WZXJDSSH_PARALLEL_COOLDOWN_MS`。历史连接标签保留当时策略。`WZXJDSSH_MAX_AUTH_TRIES=0` 表示不限认证次数，慢速阶段不限总持续时间。

### 握手探针冷却

同一来源 IP 连续 3 次 `handshake_incomplete` 后标记为探针，默认静默 15 分钟；端口不同仍合并计算。一次完整 SSH 握手立即清零，其他连接结束原因也打断连续计数，按结果完成顺序统计。期间不发送 SSH 横幅、握手或认证数据，读取并丢弃输入，到期关闭静默连接；重连不续期。标记持久化，管理设置显示阈值和冷却时长。静默连接受现有接入限制，满载时拒绝探针，不回收原有慢速连接。历史记录保留探针标签。

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
| image | ghcr.io/ettijbaqaee/wzxjdssh:2.0.4 | 固定 GHCR 镜像版本 |
| pull_policy | missing | 本地没有镜像时从 GHCR 拉取 |
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

升级步骤：修改原编排的 `image` 为 GHCR 新版本标签并重新部署，缺少镜像时自动拉取。离线升级先在 1Panel 导入新镜像，再指定对应本地标签。保留 `hostkeys:/data` 即可沿用原密钥。回滚同样通过修改镜像标签完成。删除数据卷会丢失密钥；备份和恢复时保留文件权限，并限制备份的读取权限。

从 1.x 升级到 2.0：替换编排内容时保留原编排名及 `hostkeys` 卷，新增 `records:/var/lib/wzxjdssh` 和顶层 `records` 卷声明，设置管理员密码。1.x 没有记录历史凭据，升级后才开始积累记录。此后的升级同时保留两个卷。

数据库默认路径为 `/var/lib/wzxjdssh/records.sqlite`，使用 WAL 模式。备份请在 1Panel 停止服务后备份整个 `records` 卷，或通过具备 SQLite Backup API 的工具在线备份；运行中只复制主数据库文件可能遗漏尚在 WAL 中的数据。不要把同一个数据库目录挂给多个运行实例。

清理在启动时和每小时进行。认证按提交时间清理，已结束连接按开始时间清理；仍在线连接在结束后清理。删除记录后的空闲空间可重用，不保证数据库立即缩小，也不会同步清除已有备份或快照。

若改用宿主机目录挂载 `/data`，该目录需允许容器 UID/GID `65532:65532` 读写。命名卷由 Docker 使用镜像内目录的属主和权限初始化。

升级 2.0.4：在 1Panel 拉取新版本镜像，修改原编排标签并重新创建容器，保留原密钥和数据库卷。

## 自定义配置

非空策略环境变量覆盖配置文件，命令行选项最后生效。配套编排已传入策略默认值；如需沿用挂载配置文件中的某项值，移除服务 `environment` 中的对应变量，或把编排参数设为同一值。使用 `${变量:-默认值}` 时，空值仍会展开为默认值。

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

若对端在认证延迟期间主动断开，服务记录“认证中断（可能超时）”，按来源 IP 与完整客户端 SSH 标识建立 1 小时冷却标记。服务端无法得知客户端本地的确切退出原因，因此手动取消也可能触发此标记。冷却期间再次匹配并提交认证时，先进行自适应随机等待，再返回仿真认证成功，后续 SSH 响应加入随机延迟，模拟网络波动。认证内容继续记录，公钥认证仍需客户端完成协议要求的签名。

仿真阶段以当前 `Delay` 为基准：约 75% 的出站响应延迟 `Delay/20` 至 `Delay/4`，约 25% 停顿 `Delay/2` 至 `Delay×1.5`。默认即 0.5–2.5 秒或 5–15 秒，管理设置展示实际范围；调整 `Delay` 后后续响应使用新范围。延迟基准最小为 20 毫秒。

慢速连接不限总持续时间；新连接到达且容量已满时才按需回收旧的慢速连接。客户端退出或服务停止时也会结束连接。每连接最多接受一个隔离虚拟会话，接收并丢弃输入；Shell/exec 请求仅进行协议应答，不执行命令、不读取系统文件、不启动进程。SFTP、端口转发和额外会话被拒绝。仿真阶段不再使用认证空闲超时，仍受总连接和每 IP 并发容量控制。实际延迟计入实时排行；页面区分“仿真网络迟滞”和“仿真会话结束”。

冷却到期自动恢复正常拖延，期间重连不延长到期时间。标记存于 SQLite，保留 `records` 卷即可跨重启沿用；连接详情显示标记、到期时间与仿真处理结果。历史标签按连接记录保留，过期的活动标记会被清理。

配置保存后，通过 1Panel 重启容器应用修改。若面板提供发送信号的入口，也可向服务进程发送 `SIGHUP` 进行事务重载；新配置非法或新端口无法绑定时保留旧配置。变更 Docker 端口映射属于编排变更，需要重新部署容器。单文件 bind mount 配合原子替换可能仍指向旧文件，因此示例挂载整个配置目录。

默认 `LogLevel 0` 只输出重要事件。需要连接日志时将配置设为 `LogLevel 1`，或给服务增加 `command: ["-v"]`；调试使用 `LogLevel 2` 或 `command: ["-vv"]`。

冷却重连的认证等待默认从 10 秒开始，按来源 IP 和完整客户端 SSH 标识在内存中学习：首次认证中断即按本次实际等待减去一步调整量初始化重连基准；后续等待期间提前退出，基准减少 1 秒；收到认证后的请求或通道时，增加 1 秒，每连接只调整一次。基准范围为 3–15 秒，每次等待叠加约 ±10% 随机扰动，最终实际等待仍限制在 3–15 秒。并发慢速策略优先，服务停止和容量回收不会作为失败样本。学习结果不写入 SQLite，随该标签冷却到期废弃，容器重启后从初始值重新学习；标签本身仍沿用已有持久化机制。

| 参数 | 默认值 | 作用 |
| --- | --- | --- |
| `WZXJDSSH_COOLDOWN_AUTH_DELAY_MS` | `10000` | 冷却重连认证初始等待，需在最小值和最大值之间 |
| `WZXJDSSH_COOLDOWN_AUTH_DELAY_MIN_MS` | `3000` | 实际等待下限，不得小于 3000 毫秒 |
| `WZXJDSSH_COOLDOWN_AUTH_DELAY_MAX_MS` | `15000` | 实际等待上限，不超过 60000 毫秒 |
| `WZXJDSSH_COOLDOWN_AUTH_DELAY_STEP_MS` | `1000` | 每次学习调整量，1–60000 毫秒 |
| `WZXJDSSH_COOLDOWN_AUTH_JITTER_PERCENT` | `10` | 随机扰动比例，0–50；最终仍钳在上下限 |

观测页按认证尝试、拖延统计、来源情况分组；用户名、密码、认证方式相邻，认证榜单同排，拖延与来源分栏；卡片统一固定高度，TOP 10 全部直接显示，不在卡片内滚动，手机单列，间距 12 像素。

普通认证、慢速横幅及并发慢速响应每次按 `Delay` 独立随机等待。`WZXJDSSH_DELAY_JITTER_PERCENT` 默认 `10`，范围 0–50；默认 10 秒基准对应 9–11 秒。仿真网络会话保持独立随机长短响应。超时与冷却截止按原配置计算。冷却重连中断以专用状态和关闭原因记录，不续期。

单次认证重试识别：同一 IP 与完整 SSH 标识在默认 15 分钟内连续 3 次仅提交一次密码、收到拒绝后主动退出，标为“单次认证反复重试”，冷却默认 1 小时。后续重连先自适应随机等待，再进入现有隔离仿真网络会话；不执行命令，不提供系统访问。不同来源端口继续累计，多次认证、公钥探测、认证等待中断、服务停止与容量回收不计入；不符合条件的已完成认证连接打断连续计数。同 IP 并发慢速优先，重连不延长已有冷却。触发前计数仅存内存，冷却标签持久化。

仿真认证成功只表示 SSH 已接受认证，不代表已长期留住客户端。学习增加等待的条件为 shell 或 exec 请求接受后，连接继续保持至少一个 `Delay` 基础周期（默认 10 秒）；仅开通道、发送全局请求或保活不算成功。短暂会话后主动退出会减少后续认证等待，仍遵守 3 秒下限。连接详情记录认证完成时间、认证前耗时、认证后停留、通道和会话请求阶段及底层结束信息；旧记录没有阶段数据时明确显示未采集。阶段事件每连接最多 32 条，只记动作类型与时间，不复制命令或输入正文，跟随连接记录一起清理。

冷却重连若仍在认证等待中断开，将该次真实认证等待减去一步调整量作为新的等待上限，直接下调；适用于升级重启后内存学习重置和低频重连，仍不少于 3 秒，并保持随机扰动。不延长标签冷却，不持久化学习结果，握手时间不混入样本。

首页统计口径：拖延中 IP / 连接统计仍在线且已实际进入拖延流程的来源，等待客户端下一步时仍计入；旁边另列此刻处于延迟响应计时中的数量。握手未完成及探针静默不充作已拖延。认证拒绝按已返回 rejected 的认证尝试计数；接入拦截合计限流等接入拒绝与探针静默，并列出两者。累计实际拖延合计当前保留记录和全部在线连接的实测等待，包含未结束等待，按连接编号去重；并发分别累加，不等于墙上时间，不取 TOP 10 近似值。正常重启保留已结束记录，清理旧记录后其时长也随之移除；旧版未采集及异常中断丢失的等待不推算。


仿真连接使用 SSH 应用层存活检查：默认每 60 秒发送一次 `keepalive@openssh.com`，有效的成功或失败回复均证明客户端 SSH 仍响应；TCP ACK 不算。一个待回复请求连续经过 3 个 30 秒响应窗口仍无回复时，关闭并释放连接，记录 `ssh_keepalive_timeout`。服务端主动制造的延迟不计入无响应窗口；每连接最多一个待确认请求，不随超时无限增加协程。会话达到时间门槛后还须确认 SSH 响应，才计入成功学习。FIN/RST 正常断线仍即时回收，持续响应的慢连接没有固定寿命上限。

可在编排配置 `WZXJDSSH_SSH_KEEPALIVE_INTERVAL_MS`（默认 `60000`）、`WZXJDSSH_SSH_KEEPALIVE_TIMEOUT_MS`（默认 `30000`）、`WZXJDSSH_SSH_KEEPALIVE_COUNT_MAX`（默认 `3`）。前两项范围 1–3600000 毫秒，最后一项 1–10。在线详情读取实时收发字节、最近收包和 SSH 响应时间；认证后经过时间包含双方等待，不能当作已证实客户端忙碌或实际拖延。


管理设置新增“释放全部连接 · 暂停 SSH 60 秒”：立即关闭本服务所有已接入 TCP 连接并取消 SSH 会话，60 秒内新 TCP 接入直接关闭，不发送 SSH 横幅、认证结果或协议断开消息；内核可能发送 FIN/RST。仅从应用层释放本服务连接，不修改防火墙，不新增容器权限，不影响 Web UI。重复点击不续期；倒计时属于当前进程，进程重启后重置。暂停期间新接入仅计数，不逐条写入 SQLite，避免刷拒绝记录。
