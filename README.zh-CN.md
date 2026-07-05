# LuminaCode

LuminaCode 是一个用 Go 编写的终端编码 Agent。它在当前项目目录中运行，读取本地文件和项目说明，按权限策略调用工具，并把长时间开发任务所需的会话状态保存下来，方便后续恢复。

LuminaCode 的重点不是“和模型聊天”，而是给模型提供一个真正的 Agent Runtime：项目上下文、skills、工具、记忆、MCP、会话状态和安全边界。

## Agent 能力

- 理解当前项目目录，并默认把启动目录作为工作根目录。
- 读取 `LUMINA.md` 或 `AGENTS.md` 中的项目说明，并支持用户级说明文件回退。
- 从项目、用户、旧版项目目录和安装资源中加载 reusable skills。
- 在选中 skill 后，将 skill 处理后的上下文注入模型请求，但不会污染可见对话记录。
- 调用文件和 Shell 工具，并在敏感操作前进行权限确认和安全检查。
- 支持项目声明的 MCP 服务，并把受信任 MCP 服务的 fingerprint 保存在项目根目录。
- 保存消息历史、工具结果、任务恢复信息和 skill 恢复信息，支持恢复历史会话。
- 统计上下文窗口使用情况，并基于配置的上下文长度决定本地压缩阈值。
- 支持 OpenAI-compatible 和 Anthropic-compatible 流式 API。
- 透出供应商 API 错误的原始状态码和响应体，方便定位模型或供应商配置问题。
- 将用户可见对话和内部 thinking、工具调用、工具结果等运行记录分离。

## 快速开始

安装 CLI：

```sh
make install
```

进入任意项目目录启动：

```sh
cd /path/to/project
lumina
```

执行一次性 prompt：

```sh
lumina -p "分析一下这个项目"
```

默认工作目录就是启动 `lumina` 时所在的目录。通常不需要传 `--cwd`。

## API 配置

LuminaCode 不内置默认模型。你需要通过环境变量、命令行参数或安装资源中的配置文件提供 API 信息。

常用环境变量：

```sh
export LUMINA_API_KEY="你的 API Key"
export LUMINA_API_BASE_URL="https://api.deepseek.com/anthropic"
export LUMINA_API_MODEL="deepseek-v4-pro[1m]"
export LUMINA_API_TYPE="anthropic"
```

也可以在启动时传参：

```sh
lumina \
  --api-key "$LUMINA_API_KEY" \
  --base-url "https://api.deepseek.com/anthropic" \
  --api-type anthropic \
  --model "deepseek-v4-pro[1m]" \
  --max-tokens 1000000
```

支持的 `api-type`：

- `anthropic`
- `openai_compatible`
- `auto`

`--max-tokens` 表示模型上下文窗口长度，用于本地上下文统计和压缩阈值计算。LuminaCode 会使用该值的 `80%` 作为本地压缩阈值。

API 请求不会强制携带供应商侧的 completion `max_tokens` 参数。

## 项目说明文件

LuminaCode 会优先读取当前工作目录下的说明文件：

1. `LUMINA.md`
2. `AGENTS.md`

如果当前工作目录没有这些文件，则回退读取：

1. `~/.lumina/LUMINA.md`
2. `~/.lumina/AGENTS.md`

这些文件都可以不存在。你可以用它们记录项目约定，例如测试命令、代码风格、目录结构、工具限制或领域知识。

## Skills

Skill 是一个包含 `SKILL.md` 的可复用指令包。LuminaCode 会读取以下位置：

- `{project_root}/skills/`
- `{project_root}/.Lumina/PROJECT_SKILLS/`
- `~/.Lumina/skills/`
- `~/.lumina/SKILLS/`

Skill 可以让 Agent 获得更聚焦的能力，例如代码审查、项目分析、论文写作、实验执行或项目内约定流程。调用 skill 后，它的上下文会注入到本轮模型请求中，同时保持和可见对话记录分离。

示例：

```text
/review 检查认证流程有没有安全问题
```

## 工具与权限

LuminaCode 通过工具为模型提供本地执行能力。内置工具覆盖常见编码 Agent 需求，例如读取文件、编辑文件、执行 Shell、管理任务和访问记忆。

工具执行受权限逻辑保护。敏感操作会在运行前请求人工确认。项目 MCP 服务也需要先经过信任确认，才会暴露其工具。

## MCP

项目 MCP 服务可以通过项目根目录下的 `.mcp.json` 配置。LuminaCode 会在首次使用项目 MCP 服务前请求信任确认，并将接受后的 fingerprint 写入：

```text
{project_root}/.Lumina/CONFIG/trusted_mcp.json
```

交互会话中可以使用：

```text
/mcp
```

查看已注册的 MCP 工具。

## 会话与运行数据

LuminaCode 会保存足够的运行状态来恢复历史会话，包括消息、工具结果、任务恢复数据和 skill 恢复元数据。

安装资源默认位于：

```text
~/.lumina/
```

常见内容：

- `CONFIG/defaults.json`
- `SYSTEM/system-prompt.md`
- `SYSTEM/extraction_system.md`
- `SKILLS/`

项目运行数据位于项目根目录：

```text
{project_root}/.Lumina/
```

常见内容：

- `.Lumina/worktrees/`
- `.Lumina/CONFIG/trusted_mcp.json`
- `.Lumina/PROJECT_SKILLS/`

会话历史位于：

```text
~/.Lumina/sessions/
```

注意大小写：当前实现中安装资源使用 `~/.lumina`，会话和用户 skills 使用 `~/.Lumina`。

## CLI 参数

```text
lumina [flags]
```

常用参数：

- `-p`, `-prompt`：执行一次 prompt 后退出。
- `-resume`：按会话 ID 恢复历史会话。
- `-list`：列出保存的会话。
- `-cwd`：显式指定工作目录。
- `-model`：模型名称。
- `-api-key`：API Key。
- `-base-url`：API Base URL。
- `-api-type`：`openai_compatible`、`anthropic` 或 `auto`。
- `-max-tokens`：用于本地统计的上下文窗口 token 数。
- `-yolo`：跳过权限确认。
- `-bare`：禁用 auto-memory 等持久化能力。
- `-verbose`, `-v`：开启调试输出。

fullscreen 是唯一支持的交互模式。

## 安装与卸载

安装：

```sh
make install
```

检查安装路径和 shell 配置：

```sh
make doctor
```

卸载：

```sh
make uninstall
```

`make uninstall` 会删除已安装的二进制和 `~/.lumina` 资源目录。它不会修改 shell rc 文件，也不会删除项目目录下的 `.Lumina` 数据。

## 开发

运行测试：

```sh
go test ./...
```

构建：

```sh
go build ./...
```

本地安装：

```sh
make install
```

## 目录结构

- `agent/`：Agent 主循环、状态、权限、记忆注入和工具执行。
- `agentContext/`：上下文压缩和注入流程。
- `api/`：LLM 流式客户端和供应商协议适配。
- `cli/`：slash 命令分类和补全辅助逻辑。
- `config/`：配置加载、环境变量覆盖和路径解析。
- `mcp/`：MCP 配置、信任和动态工具注册。
- `memory/`：自动记忆存储和召回。
- `security/`：命令和路径安全检查。
- `session/`：会话保存和恢复。
- `skills/`：skill 加载、解析、发现和执行。
- `tools/`：内置工具。
- `ui/`：交互运行时 frame 和终端渲染。
- `test/`：回归测试和 parity 测试。
