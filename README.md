# AtomCode2API

嗨，你好。如果你点进来了，大概是因为你也在用 AtomCode 的免费额度，想把它用到别的工具上？那来对地方了。

> **想让你的 AI agent 帮你搭？** 把 [docs/INSTALL.md](https://github.com/vibe-coding-labs/AtomCode2Api/blob/main/docs/INSTALL.md) 的完整链接发给它，AI 会读取文档并根据你的系统环境自动选择 Docker / 源码编译 / 下载二进制三种方式之一来完成安装。

## 能用哪些模型？

模型列表由你的 CodingPlan 决定，**不要写死**。用下面的命令看当前账号实际能用哪些：

```bash
curl -s http://localhost:45678/v1/models | python -m json.tool
```

实测一个 CodingPlan Lite（体验版）账号，当前暴露的是：

| 模型 | 上下文 | 视觉 | effort 等级 |
|------|--------|:----:|-------------|
| **AtomGit-glm5.3-flash** | 512,000 | ✅ | low, high |
| **AtomGit-qwen3.8-27b**（默认） | 262,144 | ✅ | low, medium, xhigh |

> ⚠️ 早期版本把 `deepseek-v4-flash` 写死成默认模型，但它**已不在 CodingPlan 目录里**。
> 更麻烦的是：daemon 收到未知模型名时**不报错，而是静默回退到默认模型**，
> 所以你以为在用 DeepSeek，实际跑的是另一个模型。
> 现在代理对未知模型返回 404 并列出可用模型，不再静默降级。

## 这东西是干嘛的

简单说就是：**AtomCode 有个免费版（CodingPlan Lite），每个月能免费用很多次 AI。但 Claude Code、Cursor、Codex 这些工具连不上它，因为 AtomCode 用的是自己的私有协议，不是标准的 OpenAI 接口。**

AtomCode2API 就是一个"翻译官"——它坐在你的工具和 AtomCode 之间，把工具发的标准请求翻译成 AtomCode 能听懂的话，再把 AtomCode 的回复翻译回来。

所以，你不需要再花钱买 OpenAI 的 API Key，直接用 AtomCode 的免费额度就够了。

## 大概长这样

装好之后，你会有个 Web 管理面板，打开浏览器就能看到：

```
http://localhost:45678/
```

里面有这么几个页面：

- **数据概览** — 今天发了多少请求、用了多少 Token、成功失败了几个，图表都有
- **账号管理** — 你的 AtomCode 账号列表，可以一键导入，也能手动添加
- **账号详情** — 点进某个账号，能看到：
  - 复制即用的 Claude Code / Codex 启动命令（不用自己拼环境变量）
  - 可用模型列表（哪个免费哪个付费，价格多少）
  - 请求日志（什么时候调的哪个模型，延迟多少）
  - 套餐信息（你的 CodingPlan 啥时候到期，还剩多少天）
- **系统设置** — 超时时间、日志开关、改密码

## 解决了什么问题

你有 AtomCode 的免费额度，但你想用 Claude Code 或者 Codex 或者 Cursor 来写代码。这些工具只认 OpenAI 和 Anthropic 的标准接口，不认 AtomCode 的私有协议。

之前你可能得：
1. 再掏钱买 OpenAI 的 API Key
2. 或者在几个工具之间来回切换

现在不用了，装个 AtomCode2API 就行。

## 怎么装

### 前提

你已经装好了 [AtomCode](https://atomcode.atomgit.com/)，登录了，并且在跑着：

```bash
atomcode daemon --port 13456 --idle-timeout 0
```

### 方式一：本地跑

```bash
git clone https://github.com/vibe-coding-labs/AtomCode2Api.git
cd AtomCode2Api
go build -o atomcode-2api ./cmd/atomcode-2api/
./atomcode-2api serve -v
```

然后打开 http://localhost:45678/ 就能看到管理面板了。

### 方式二：Docker 跑

```bash
docker build -t atomcode-2api .
docker run -d --name atomcode-2api \
  --add-host host.docker.internal:host-gateway \
  -p 45678:45678 \
  atomcode-2api
```

## 怎么用

### 配置 Claude Code

```bash
export ANTHROPIC_BASE_URL=http://localhost:45678
export ANTHROPIC_API_KEY=sk-atmc-xxxxx
export ANTHROPIC_MODEL=<你的模型名>
claude
```

以上三个环境变量在管理面板的账号详情页有一键复制按钮，点一下就能复制完整命令。

### 配置 Codex

```bash
export OPENAI_BASE_URL=http://localhost:45678/v1
export OPENAI_API_KEY=sk-atmc-xxxxx
export OPENAI_MODEL=<你的模型名>
codex exec "你的问题"
```

### 配置 Cursor

在 Cursor Settings → Models 里填：
- API Base URL: `http://localhost:45678/v1`
- API Key: 从管理面板复制
- Model: `<你的模型名>`

## 工具调用（Tool Calling）

Claude Code / Cursor / Codex 这类客户端靠工具调用干活。但 **AtomCode daemon 会忽略请求里的 `tools` 字段**，只暴露它自己的内置工具，所以没法直接透传。

AtomCode2API 在中间做了一层桥接：把客户端声明的工具以文本协议注入提示词，再把模型回复解析回标准的 `tool_calls` / `tool_use`。

```
客户端发 tools ──► 代理注入协议提示词 ──► daemon ──► 模型返回 <tool_call> 块
                                                              │
客户端本地执行工具 ◄── 代理解析为标准 tool_calls ◄─────────────┘
        │
        └── 回传结果 ──► 代理渲染为 TOOL RESULT ──► daemon 继续作答
```

已经处理的情况：

- 流式输出时扣留协议标记，客户端不会看到 `<tool_call>` 原文
- 模型编造的工具名会被丢弃，不会转发给客户端
- 兼容 markdown 代码块包裹、`parameters`/`input` 等变体字段名、`name({...})` 调用写法
- 未声明工具时完全不走桥接，纯文本直通，零影响
- 多轮：`assistant.tool_calls` 与 `role: "tool"` 结果都会渲染进 daemon 请求历史

`tool_choice: "none"` 关闭桥接，`"required"`/`"any"` 强制本轮必须调用工具。

> 注意：桥接依赖模型遵循文本协议。协议提示词会占用一部分上下文，且模型偶尔可能不按格式输出——此时会退化成普通文本回复，不会报错。

## 代码结构

```
cmd/atomcode-2api/    命令行工具（启动、设置、登录等）
pkg/atmc/             和 AtomCode Daemon 通信的客户端
pkg/openai/           OpenAI 协议翻译
pkg/anthropic/        Anthropic 协议翻译
pkg/toolbridge/       工具调用桥接（文本协议注入 + 解析）
pkg/store/            SQLite 数据库（存账号、设置、请求日志）
pkg/auth/             认证相关
pkg/dashboard/        Web 管理面板
web/                  前端页面（React + Ant Design）
```

## 说在最后

这个项目是开源的（Apache 2.0），如果你觉得有用，欢迎 star。如果你遇到问题，提 issue 就行。

能用免费额度解决的问题，何必花钱呢？