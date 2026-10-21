# 飞书入口：部署与安全

飞书只是请求和回复渠道，Agent 的上下文、模型、工具与运行记录仍由 Go 运行时处理。一个进程绑定一个专用工作区；白名单中的用户都可以读取该工作区，因此不是多租户数据隔离服务。

## 启动前准备

1. 执行 `make build`，得到 `bin/gopherharness-gateway`。
2. 准备专用工作区及 `.pico.toml` 或模型环境变量。模型配置方式与 CLI 相同；入口启动前要求提供 API key。
3. 在飞书应用启用机器人、配置消息接收事件和读取/发送权限，使用事件订阅的 HTTP 回调方式，不是长连接模式。
4. 配置事件加密密钥、验证令牌、机器人 `open_id` 和调用者 `open_id` 白名单。应用可见范围、管理员授权和平台权限也必须满足。
5. 用 HTTPS 反向代理把公网回调映射到本地 `/events/feishu`。默认只监听回环地址，不要直接暴露不加密的 HTTP 端口。

```sh
cp examples/feishu.env.example .env.feishu
# 编辑 .env.feishu，替换全部占位值
set -a
. ./.env.feishu
set +a
./bin/gopherharness-gateway --cwd /path/to/dedicated-workspace \
  --listen 127.0.0.1:8080 --user-slots 2 --system-slots 1 \
  --busy append --max-steps 20 --model-timeout 120
```

`.env.feishu` 和 `.pico.toml` 已被 Git 忽略。不要提交真实凭证、运行目录或用户聊天记录。环境文件由你自己创建和加载，程序不会自动执行其中内容。

## 请求链路与权限

```text
加密回调 → 官方 SDK 解密/验签 → 时间/大小检查
  → 白名单、群提及和机器人自身过滤 → 规范事件 → 持久 Inbox
  → 会话调度 → 独立 Session → Agent 工具循环
  → Inbox 完成 + Outbox 待发（同一事务）
  → Delivery 有界队列 → 飞书发送接口 → 保存回执
```

- 私聊无需提及；群聊和话题群必须提及已配置的机器人。
- 会话键包含租户、群、话题、发送者，避免不同用户对话历史混合；回复目标始终取原始群地址，不由模型决定。
- `append` 顺序追加；`inject` 在下一次模型调用前注入，不改动执行中的工具；`interrupt` 请求取消当前任务并等待结束，再执行新请求。取消不撤销已发生的外部动作。
- 仅允许 `read_file`、`list_files`、`search`。不开放 Shell、写文件、子任务或任意 MCP 工具，聊天指令不能提升权限。本地 CLI/TUI 的策略独立。
- 图片、文件、音频和媒体可下载为工作区附件，上限 10 MiB；文本和富文本可转为输入。不代表自动具备视觉理解、语音识别或富文本中全部附件解析。
- 长回复截断并提示查看本地运行记录，不自动上传完整文件。

## 恢复与发送失败

`.pico/gateway/journal.json` 包含规范消息和回复，需按敏感资料管理。文件锁保护单机所有者，代际校验阻止过期持有者写入；不能多台机器共享该文件来实现分布式选主。

重启只自动重排 `pending`（明确未启动）的工作和回复。`running` 转 `needs_reconcile`（需对账），`sending` 转 `unknown`（结果未知），不会盲目再跑 Agent。当前无交互式对账命令，需要检查记录与平台状态后人工处理。

发送用结果标识生成稳定 `uuid`，有界重试复用同一标识，HTTP 明确不可恢复错误停止重试。平台幂等窗口不是无限期保证，不能宣称所有崩溃窗口都绝不重复。发送失败不会重新调用模型。

退出时先停止 HTTP 接收，再封闭调度、取消并等待任务、清理投递、关闭 Agent 与账本，最后释放持久日志锁。超时请求取消，Go 无法强制终止不响应上下文的任意协程。

## 本地验证

```sh
go test -race -count=1 ./internal/feishu ./internal/gatewayapp ./internal/gateway ./internal/delivery ./internal/journal
```

测试覆盖加密回调、挑战确认、错误签名/时间、白名单、群提及、只读工具、去重、重启、媒体上限与取消、稳定发送标识、令牌缓存和关闭清理，均使用本地模拟服务与固定模型响应。

`GET /healthz` 只确认 HTTP 存活，不证明模型、应用权限、外网或投递正常。真实群聊、公网代理和真实凭证需在你的授权环境另行验收，测试不会执行这些外部操作。
