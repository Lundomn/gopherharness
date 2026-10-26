# 飞书入口：部署与安全

飞书只是请求和回复渠道，Agent 的上下文、模型、工具与运行记录仍由 Go 运行时处理。一个进程绑定一个专用工作区；白名单中的用户都可以读取该工作区，因此不是多租户数据隔离服务。

## 启动前准备

1. 执行 `make build`，得到 `bin/gopherharness-gateway`。
2. 准备专用工作区及 `.pico.toml` 或模型环境变量。模型配置方式与 CLI 相同；入口启动前要求提供 API key。
3. 在飞书应用启用机器人，配置消息接收事件和读取/发送权限。网关默认使用官方 SDK 长连接，飞书侧选择长连接订阅；需要 HTTP 回调时显式指定 `--transport webhook`。
4. 配置应用 ID、应用密钥、机器人 `open_id` 和调用者 `open_id` 白名单；可用 `FEISHU_ALLOWED_CHATS` 再限定群聊。HTTP 回调模式还必须配置事件加密密钥和验证令牌。应用可见范围、管理员授权和平台权限也必须满足。
5. 长连接模式由网关主动连接飞书，不要求公网回调地址。HTTP 回调模式使用 HTTPS 反向代理将公网请求转发到 `/events/feishu`；网关默认只监听回环地址，不要直接暴露不加密的 HTTP 端口。

```sh
cp examples/feishu.env.example .env.feishu
# 编辑 .env.feishu，替换全部占位值
set -a
. ./.env.feishu
set +a
./bin/gopherharness-gateway --cwd /path/to/dedicated-workspace \
  --transport websocket --listen 127.0.0.1:8080 --user-slots 2 --system-slots 1 \
  --busy append --max-steps 20 --model-timeout 120
```

`.env.feishu` 和 `.pico.toml` 已被 Git 忽略。不要提交真实凭证、运行目录或用户聊天记录。环境文件由你自己创建和加载，程序不会自动执行其中内容。

改用加密 HTTP 回调时，将上述 `--transport websocket` 改为 `--transport webhook`，并在飞书侧同步切换订阅方式。长连接模式下 `/events/feishu` 返回 404，避免同时启用另一条消息入口。

## 请求链路与权限

```text
官方 SDK 长连接，或加密回调解密/验签与时间/大小检查
  → 白名单、群提及和机器人自身过滤 → 规范事件 → 持久 Inbox
  → 会话调度 → 独立 Session → Agent 工具循环
  → Inbox 完成 + Outbox 待发（同一事务）
  → Delivery 有界队列 → 飞书发送接口 → 保存回执
```

- 私聊无需提及；群聊和话题群必须提及已配置的机器人。
- 会话键包含租户、群、话题、发送者，避免不同用户对话历史混合；回复目标始终取原始群地址，不由模型决定。
- `append` 顺序追加；`inject` 在下一次模型调用前注入，不改动执行中的工具；`interrupt` 请求取消当前任务并等待结束，再执行新请求。取消不撤销已发生的外部动作。
- 默认只允许 `read_file`、`list_files`、`search`。宿主可通过 `--allow-tools` 显式调整允许列表；风险工具仍须经过已配置的审批策略，远程自动批准会被拒绝。聊天指令不能提升权限，本地 CLI/TUI 的策略独立。
- 图片、文件、音频和媒体可下载为工作区附件，上限 10 MiB；文本和富文本可转为输入。不代表自动具备视觉理解、语音识别或富文本中全部附件解析。
- 长回复截断并提示查看本地运行记录，不自动上传完整文件。

## 恢复与发送失败

`.pico/gateway/journal.json` 包含规范消息和回复，需按敏感资料管理。文件锁保护单机所有者，代际校验阻止过期持有者写入；不能多台机器共享该文件来实现分布式选主。

重启只自动重排 `pending`（明确未启动）的工作和回复。`running` 转 `needs_reconcile`（需核对执行结果），`sending` 转 `unknown`（需核对投递结果），不自动再跑 Agent。核对已有运行记录与平台回执后，通过本地命令处理；先停止网关，命令才能取得日志所有权锁。

```sh
# 只列出需要核对的记录，ID 使用输出中的完整值（包含渠道前缀）
./bin/gopherharness-gateway --cwd /path/to/dedicated-workspace --reconcile list
```

| 核对结果 | 对账动作与后续步骤 |
| --- | --- |
| 平台已收到回复，且有消息 ID | `--reconcile delivered --record-kind outbox --receipt <消息ID>`：补存回执，保持已送达。 |
| 已确认平台没有收到回复 | `--reconcile confirmed_not_sent --record-kind outbox`：恢复待发送，下一次启动只重发已保存的回复。 |
| 已从运行记录核验完整结果，原请求因中断未完成记账 | `--reconcile recover_result --record-kind inbox --result-file <结果文本文件>`：同一事务保存完成状态与待发回复，沿用原请求的回复地址。 |
| 决定终止处理该记录 | `--reconcile abandon --record-kind inbox` 或 `outbox`：标记放弃，停止自动处理。 |

每个修改动作还须带上 `--record-id`、`--operator`、`--reason` 和 `--evidence`，保存操作者、原因及核对依据。命令只接受对应的未决状态；证据不足时保留未决记录，不修改日志文件来强行重试。恢复结果与补发回复都不重新调用模型或执行工具。

发送用结果标识生成稳定 `uuid`，有界重试复用同一标识，HTTP 明确不可恢复错误停止重试。平台幂等窗口不是无限期保证，不能宣称所有崩溃窗口都绝不重复。发送失败不会重新调用模型。

退出时先停止消息接收并关闭长连接，再停止调度、取消并等待任务、清理投递、关闭 Agent 与账本，最后释放持久日志锁。超时请求取消，Go 无法强制终止不响应上下文的任意协程。

## 本地验证

```sh
go test -race -count=1 ./internal/feishu ./internal/gatewayapp ./internal/gateway ./internal/delivery ./internal/journal
```

测试覆盖官方 SDK 长连接接入、重连就绪状态、加密回调、挑战确认、错误签名/时间、白名单、群提及、远程工具权限、去重、重启、人工对账、媒体上限与取消、稳定发送标识、令牌缓存和关闭清理，均使用本地模拟服务与固定模型响应。

`GET /healthz` 确认 HTTP 存活；`GET /readyz` 检查服务是否正在关闭，并在长连接模式下检查 SDK 的连接就绪状态。二者不替代真实模型、应用权限和投递验证。真实群聊、公网代理和真实凭证需在授权环境另行验收，测试不会执行这些外部操作。
