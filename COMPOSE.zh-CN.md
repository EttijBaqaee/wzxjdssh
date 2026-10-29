# wzxjdssh 1.0.2：1Panel 编排使用文档

配套文件：[compose.release.yaml](compose.release.yaml)。镜像为 `wzxjdssh:1.0.2`，支持 Linux amd64。默认公开标识为 `SSH-2.0-OpenSSH_10.0p2 Debian-7+deb13u4`，可以通过编排环境参数修改。

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
| image | wzxjdssh:1.0.2 | 指定本地镜像版本 |
| pull_policy | never | 使用本地镜像，不从远端拉取 |
| container_name | wzxjdssh | 固定容器名；同一宿主机部署多份时需分别命名或移除此字段 |
| restart | unless-stopped | 自动恢复容器；手动停止后保持停止 |
| volumes | hostkeys:/data | 保存服务端主机私钥，重建容器时继续使用 |
| read_only | true | 根文件系统只读；密钥数据卷仍可写 |
| cap_drop | ALL | 移除 Linux capabilities |
| security_opt | no-new-privileges:true | 禁止进程获得额外权限 |
| mem_limit | 256m | 内存上限，超过时可能被终止；按连接量调整 |
| cpus | 1 | CPU 配额上限；真实密钥交换会消耗 CPU |
| pids_limit | 256 | 限制容器内任务数量；不是连接数限制 |
| ulimits.nofile | 16384 | 文件描述符软、硬上限 |
| logging | json-file，10m × 3 | 限制 Docker 日志文件大小及保留数量 |

镜像内用户为 UID/GID `65532:65532`，健康检查调用内置 `/wzxjdssh -health`。正式镜像基于 scratch，没有 shell、包管理器或编译器，使用面板的容器日志查看运行情况。

`MaxClients` 默认是 4096，属于接入限制，并非这套资源配额已验证的容量。需要结合实际握手速率、内存和 CPU 调整。

## 密钥卷与升级

`hostkeys` 是 Docker 命名卷；默认实际卷名包含编排项目名。因此，升级时应编辑原有编排，保留同一个编排名和卷声明。另建编排通常会创建新卷，从而生成不同主机密钥。

升级步骤：先在 1Panel 导入新镜像，再修改原编排的 `image` 标签并重新部署。保留 `hostkeys:/data` 即可沿用原密钥。回滚同样通过修改镜像标签完成。删除数据卷会丢失密钥；备份和恢复时保留文件权限，并限制备份的读取权限。

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

配置保存后，通过 1Panel 重启容器应用修改。若面板提供发送信号的入口，也可向服务进程发送 `SIGHUP` 进行事务重载；新配置非法或新端口无法绑定时保留旧配置。变更 Docker 端口映射属于编排变更，需要重新部署容器。单文件 bind mount 配合原子替换可能仍指向旧文件，因此示例挂载整个配置目录。

默认 `LogLevel 0` 只输出重要事件。需要连接日志时将配置设为 `LogLevel 1`，或给服务增加 `command: ["-v"]`；调试使用 `LogLevel 2` 或 `command: ["-vv"]`。
