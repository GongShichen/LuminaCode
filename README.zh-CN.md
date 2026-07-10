# LuminaCode

LuminaCode 是一个本地运行的通用 Agent。它由 Go 后端和 TypeScript TUI 组成，提供项目上下文、工具、skills、MCP、记忆、session 和 Agent Team。

## Agent 能力

- 默认把启动目录作为项目根目录。
- 读取 `LUMINA.md` / `AGENTS.md`，并支持 `~/.lumina` 用户级回退。
- 加载项目、用户和内置 skills。
- 调用文件、Shell、Web、MCP、记忆和任务工具，并带权限控制。
- 保存可恢复 session：transcript、state、tasks、tool results、skill recovery 和 session memory。
- 支持 OpenAI-compatible 和 Anthropic-compatible 流式 API。
- 将可见对话与工具 payload、tool result、runtime 记录分离。
- 支持 sub-agent、Agent Team、headless 和 benchmark harness。

## 架构

安装后会有两个命令：

- `lumina`：TypeScript 终端前端，也是默认用户入口。
- `lumina-backend`：Go runtime，负责 Agent 执行、工具、skills、MCP、记忆、session、headless 和 benchmark 模式。

交互会话通过 localhost WebSocket 通信：

```text
~/.lumina/run/backend.json
```

后端只监听 `127.0.0.1`，连接必须携带 auth token；支持多个 session，同一个 session 内串行 submit。Headless 路径由 `lumina-backend` 处理。

## Agent Team

Agent Team 模式让一组互相隔离的专家 Agent 协作完成任务。每个成员都有独立 prompt、skills、上下文、任务状态和 A2A inbox/outbox，同时复用同一套 backend runtime 和工具系统。

命令：

```text
/Team      选择已安装 Team 并进入 Team 模式
/TeamOut   退出 Team 模式
/NewTeam   创建一个新的可编辑 Team 模板
```

TUI 会像群聊一样展示 Team 对话。原始 tool payload、完整 tool result、MCP payload 和隐藏 reasoning 只进入 runtime 日志。

Runtime 要点：

- Loop：observe -> plan -> dispatch -> agent work -> collect -> gate -> finalize。
- 停止条件：用户打断或任务完成。
- 失败会进入下一轮恢复，而不是静默成功。
- 普通 Agent 上下文和 Team Agent 上下文隔离。

```text
{session_dir}/{parent_session_id}/teams/{team_session_id}/
{session_dir}/{parent_session_id}/teams/{team_session_id}/agents/{agent_id}/
~/.lumina/project/{project_root_name}/teams/{team_name}/{team_session_id}/
```

### 内置 Team

安装位置：`~/.lumina/TEAM/`

- `product-development`：全栈开发 Team，包含 `team-leader`、`research`、`frontend`、`backend`、`qa`、`reviewer`、`devops`、`ux-design`。启用 contract、QA、Reviewer、task policy 和 follow-up/deferral gate。
- `deep-research`：研究 Team，包含 `team-leader`、`scope-planner`、`search-strategist`、`source-reader`、`evidence-analyst`、`report-writer`、`qa`、`reviewer`。使用 SearxNG `WebSearch` / `WebFetch` 和 arXiv MCP，可导出报告与证据文件。

### 创建新的 Team

`/NewTeam` 会询问展示名并创建：

```text
~/.lumina/TEAM/{team_name}/
├── team.yaml
├── team-system.md
├── shared-prompt.md
├── completion-policy.md
└── team-leader/
    ├── agent.yaml
    ├── system.md
    └── skills/
```

模板默认只有 `team-leader`。新增成员时创建新的 agent 目录，并把 id 写入 `team.yaml`。

### Team 配置文件

最小 `team.yaml` 结构：

```yaml
name: my-team
display_name: My Team
entry_agent: team-leader
loop:
  max_iterations: 0
  max_parallel_agents: 2
  completion_policy: team_leader_only
  stop_policy: user_interrupt_or_task_complete_only
gates:
  require_contract: false
  checks: []
transcript:
  show_member_dialogue: true
  show_tool_details: false
  show_thinking: false
agents:
  - team-leader
```

Agent `agent.yaml`：

```yaml
name: team-leader
display_name: Team Leader
communicates_with: all
model: inherit
tools: inherit
max_turns_per_task: 0
private_skills: true
```

`communicates_with` 可以是 `all` 或 agent id 列表。专属 skill 放在该 agent 的 `skills/` 目录。

## 快速开始

安装 CLI：

```sh
# macOS/Linux
make install
```

Windows PowerShell：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\install-windows.ps1
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

列出历史 session：

```sh
lumina --list
```

恢复历史 session：

```sh
lumina --resume <session-id>
```

启动目录就是默认工作目录。

## API 配置

不内置默认模型。通过环境变量、命令行参数或 `~/.lumina/CONFIG/defaults.json` 配置：

```sh
export LUMINA_API_KEY="你的 API Key"
export LUMINA_API_BASE_URL="https://api.deepseek.com/anthropic"
export LUMINA_API_MODEL="deepseek-v4-pro[1m]"
export LUMINA_API_TYPE="anthropic"
```

也兼容 `LLM_*`，但 `LUMINA_*` 优先。等价参数：

```sh
lumina \
  --api-key "$LUMINA_API_KEY" \
  --base-url "https://api.deepseek.com/anthropic" \
  --api-type anthropic \
  --model "deepseek-v4-pro[1m]" \
  --max-tokens 1000000
```

`api-type`：`anthropic`、`openai_compatible`、`auto`。

`--max-tokens` 是本地上下文窗口长度，用于统计和 80% 压缩阈值。API 请求不会强制携带供应商侧 completion `max_tokens`。runtime 配置会在每轮 Agent 请求前热读取。

## 项目说明文件

读取顺序：

1. `{cwd}/LUMINA.md`
2. `{cwd}/AGENTS.md`
3. `~/.lumina/LUMINA.md`
4. `~/.lumina/AGENTS.md`

这些文件都可以不存在。

## Skills

Skill 是包含 `SKILL.md` 的指令包，读取位置：

- `{project_root}/skills/`
- `{project_root}/.Lumina/PROJECT_SKILLS/`
- `~/.lumina/skills/`
- `~/.lumina/SKILLS/`

Skill 上下文会注入本轮模型请求，但不进入可见对话。

```text
/review 检查认证流程有没有安全问题
```

## 工具与权限

工具覆盖文件编辑、Shell、任务、记忆、WebSearch/WebFetch 和 MCP。敏感操作和项目 MCP 可要求人工确认。

## MCP

项目 MCP 配置：`.mcp.json`。信任记录：

```text
~/.lumina/project/{project_root_name}/CONFIG/trusted_mcp.json
```

使用 `/mcp` 查看已注册 MCP 工具。

## 会话与运行数据

安装资源：

```text
~/.lumina/
```

- `CONFIG/defaults.json`
- `SYSTEM/system-prompt.md`
- `SYSTEM/extraction_system.md`
- `SKILLS/`
- `TEAM/`
- `frontend/`

项目级 runtime 数据：

```text
~/.lumina/project/{project_root_name}/
```

- `CONFIG/trusted_mcp.json`
- `agent-memory/`
- `agent-memory-local/`
- `background/tool-results/`

项目内用户资源：

- `{project_root}/skills/`
- `{project_root}/.Lumina/PROJECT_SKILLS/`

会话历史使用 `session_dir`，默认 `~/.lumina/sessions`：

```text
{session_dir}/{session_id}/
```

- `transcript.jsonl`
- `transcript.md`
- `meta.json`
- `state.json`
- `tasks.json`
- `skill-recovery.json`
- `skill-recovery.commit.json`
- `session.sqlite`

大型后台输出：

```text
~/.lumina/project/{project_root_name}/background/tool-results/
```

## CLI 参数

```text
lumina [flags]
```

`lumina` 启动 TS 前端。也可以直接调用 Go 后端：

```sh
lumina-backend -p "分析一下这个项目"
lumina-backend --list
lumina-backend daemon --host 127.0.0.1 --port 0
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
- `-yolo`：跳过权限确认和操作系统沙箱隔离。
- `-bare`：禁用 auto-memory 等持久化能力。
- `-verbose`, `-v`：开启调试输出。

交互模式使用 TypeScript 前端；headless 模式使用 `lumina` passthrough 或 `lumina-backend`。

## 安装与卸载

macOS/Linux：

```sh
make install
```

Windows：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\install-windows.ps1
```

Doctor：

```sh
make doctor
```

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\doctor-windows.ps1
```

卸载：

```sh
make uninstall
```

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\uninstall-windows.ps1
```

`make uninstall` 会关闭 backend/MCP/SearxNG，删除安装命令和 `~/.lumina`，但不修改 shell rc，也不删除项目内 `.Lumina`。

## 开发

测试：

```sh
go test ./...
npm --prefix frontend test
```

构建：

```sh
# macOS/Linux
make build
```

安装：

```sh
# macOS/Linux
make install
```

Windows 源码运行：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\setup-windows.ps1
.\tmp\lumina.cmd --help
```

## 目录结构

- `agent/`：Agent 主循环、状态、权限、记忆注入和工具执行。
- `agentContext/`：上下文压缩和注入流程。
- `api/`：LLM 流式客户端和供应商协议适配。
- `backend/`：WebSocket daemon、session manager 和前端 IPC bridge。
- `cli/`：slash 命令分类和补全辅助逻辑。
- `config/`：配置加载、环境变量覆盖和路径解析。
- `frontend/`：TypeScript 终端前端。
- `mcp/`：MCP 配置、信任和动态工具注册。
- `memory/`：自动记忆存储和召回。
- `security/`：命令和路径安全检查。
- `session/`：会话保存、迁移和恢复。
- `sessionmemory/`：per-session memory commit log 和历史查询工具。
- `skills/`：skill 加载、解析、发现和执行。
- `team/`：Agent Team 配置、runtime loop、A2A 对话、gate 和持久化。
- `tools/`：内置工具。
- `ui/`：共享 runtime frame model 和旧 renderer 回归测试。
- `test/`：回归测试和 parity 测试。
