# LuminaCode

LuminaCode 是一个本地运行的通用 Agent。它由 Go 后端和 TypeScript TUI 组成，提供项目上下文、工具、skills、MCP、记忆、session 和 Agent Team。

## Agent 能力

- 默认把启动目录作为项目根目录。
- 读取 `LUMINA.md` / `AGENTS.md`，并支持 `<AppRoot>/config/instructions` 用户级回退。
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
<AppRoot>/state/run/backend.json
```

后端只监听 `127.0.0.1`，连接必须携带 auth token；支持多个 session，同一个 session 内串行 submit。Headless 路径由 `lumina-backend` 处理。

## 长期记忆

Memory Fabric 使用两个本地 SQLite 数据库保存持久化跨 Session 状态，并构建
一个可替换的 BGE-M3 检索 sidecar：

```text
<AppRoot>/data/memory/fabric/ledger.sqlite
<AppRoot>/data/memory/fabric/index.sqlite
<AppRoot>/data/memory/fabric/retrieval-bge-m3.sqlite
```

### 记忆写入流程

1. **持久化原始证据**：先把可见的 user、assistant 和 tool event 写成不可变、
   可定位来源的 ledger row，再进行任何语义处理。
2. **绑定上下文与 provenance**：记录 project space、context、session、actor、
   时间、turn 顺序、checksum 和 source offset，确保每条事件都能还原。
3. **语义编译**：可选地使用配置的 API 模型筛选持久记忆，并生成有原文依据的
   node、identity、slot、时间 scope、检索线索和冲突候选。每个 node 都必须
   引用 source event 并通过本地 grounding 校验。
4. **冲突处理**：明确更新由本地权威和时间规则处理；歧义情况保持 pending，
   或使用配置的 adjudicator。原始证据永远不会被覆盖。
5. **本地 BGE-M3 索引**：生成 dense vector，以及检索需要的 event/span dense、
   learned sparse、FTS 和图表示。
6. **可恢复发布**：后台任务支持 checkpoint，派生索引原子发布。模型、
   tokenizer 或 schema 变化时从 ledger 重建派生数据，无需重新写入对话。

### 记忆检索流程

检索完全在本地执行，所有普通自然语言查询统一走同一条管线：

1. **对齐检查**：验证 sidecar 的模型、tokenizer、schema 和 ledger checksum，
   必要时增量补齐缺失事件。
2. **Query encoding**：生成 BGE-M3 dense、learned sparse 和全 token 表示。
3. **候选召回**：span FTS5、event dense 和 learned sparse 三个通道各召回
   最多 128 个候选。
4. **融合与图扩散**：reciprocal-rank fusion 形成召回池，再对 event、context、
   semantic node 和 sparse concept 图执行 Personalized PageRank。
5. **精确 span 打分**：使用 BGE-M3 dense、learned sparse 和全 token ColBERT
   MaxSim，融合权重固定为 `1.0 : 0.3 : 1.0`。
6. **证据选择**：按 source event 去重，再用 submodular objective 在配置的
   evidence 数量和 token 预算内最大化相关性与覆盖。
7. **上下文组织**：应用 reference time 与冲突状态过滤，对证据进行一致分组，
   并为回答模型保留时间、actor、context、source ID 和 span 位置。

ID、路径和引用文本只作为 lexical candidate feature，query 内容不会绕过
BGE-M3。生产检索没有远程 query expansion、题型路由、benchmark 实体、本地
答案 bypass。

### LongMemEval-S

在 LongMemEval-S 500 题完整 haystack 评测中，LuminaCode Memory Fabric
使用本地 BGE-M3 检索，并以 `mimo-v2.5-pro` 作为回答模型和 official
evaluator，取得 **83.00%（415/500）**。

| 题型 | 正确数 | 准确率 |
|---|---:|---:|
| Knowledge update | 74/78 | 94.87% |
| Single-session user | 65/70 | 92.86% |
| Temporal reasoning | 114/133 | 85.71% |
| Multi-session | 103/133 | 77.44% |
| Single-session assistant | 40/56 | 71.43% |
| Single-session preference | 19/30 | 63.33% |

下表来自本轮 500 份 diagnostics；prepared-sidecar latency 不包含一次性
sidecar 同步：

| 检索指标 | 平均 | P50 | P95 |
|---|---:|---:|---:|
| Sidecar 已准备时的检索 | 8.25 秒 | 7.17 秒 | 17.20 秒 |
| Query encoding | 2.56 秒 | 1.72 秒 | 8.44 秒 |
| Personalized PageRank | 0.91 秒 | 0.84 秒 | 1.53 秒 |
| Evidence selection | 1.80 秒 | 1.69 秒 | 2.89 秒 |
| Sidecar 缺失时的同步 | 343.09 秒 | 394.79 秒 | 458.27 秒 |

公开 LongMemEval 成绩按分数排序如下，仅用于定位：

| 系统 | 准确率 | 公开评测设置 |
|---|---:|---|
| Exabase M-1 | 96.4% | Gemini 3 Flash，Top 50；厂商公开结果 |
| Mastra Observational Memory | 94.87% | GPT-5-mini；实现和 runner 开源 |
| Mem0 Platform | 94.8% | Mem0 当前 benchmark，Top 50 |
| Honcho | 92.6% | 公开报告；完整运行配置未披露 |
| Engram | 91.6% | GPT-5 composer、GPT-4o judge；公开 prompt 和运行产物 |
| Hindsight | 91.4% | Gemini 3 Pro；公开 benchmark 仓库 |
| HydraDB | 90.79% | Gemini 3 Pro；论文报告 |
| **LuminaCode（LongMemEval-S）** | **83.0%** | 完整 haystack；`mimo-v2.5-pro` 回答及 official evaluator；完整 500 题 |
| LiCoMemory | 73.8% | GPT-4o-mini，5 次均值 |
| Mem0-G | 64.8% | GPT-4o-mini 同设置 baseline |
| Mem0 | 62.6% | GPT-4o-mini 同设置 baseline |
| Zep | 58.6% | GPT-4o-mini 同设置 baseline |
| A-Mem | 55.0% | GPT-4o-mini 同设置 baseline |
| MemOS | 51.2% | GPT-4o-mini 同设置 baseline |

来源：[LongMemEval](https://github.com/xiaowu0162/longmemeval)、
[Mem0 benchmark](https://github.com/mem0ai/memory-benchmarks)、
[LiCoMemory 论文](https://aclanthology.org/2026.findings-acl.1835/)、
[Mastra Observational Memory](https://mastra.ai/research/observational-memory)、
[Hindsight benchmark](https://github.com/vectorize-io/hindsight-benchmarks)、
[Engram benchmark](https://lumetra.io/engram-on-longmemeval/)、
[HydraDB 论文](https://research.hydradb.com/hydradb.pdf)、
[Honcho](https://github.com/plastic-labs/honcho) 以及
[Exabase M-1 公告](https://www.prnewswire.com/news-releases/exabase-achieves-highest-reported-score-on-leading-ai-memory-benchmark-using-a-smaller-cheaper-model-302780919.html)。
Exabase 和 Honcho 使用公开报告分数，但完整复现材料相对有限。不同报告的
reader、检索深度、上下文预算、回答模型和 judge 不完全一致，因此这不是严格
同口径排行榜。

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
<AppRoot>/data/projects/{project-id}/teams/{team_name}/{team_session_id}/
<AppRoot>/data/projects/{project-id}/teams/{team_name}/{team_session_id}/agents/{agent_id}/
```

### 内置 Team

安装位置：`<AppRoot>/app/resources/teams/`

- `product-development`：全栈开发 Team，包含 `team-leader`、`research`、`frontend`、`backend`、`qa`、`reviewer`、`devops`、`ux-design`。启用 contract、QA、Reviewer、task policy 和 follow-up/deferral gate。
- `deep-research`：研究 Team，包含 `team-leader`、`scope-planner`、`search-strategist`、`source-reader`、`evidence-analyst`、`report-writer`、`qa`、`reviewer`。使用 SearxNG `WebSearch` / `WebFetch` 和 arXiv MCP，可导出报告与证据文件。

Team 读取顺序为：项目 `.Lumina/TEAM`、用户 `<AppRoot>/config/teams`、安装内置
`<AppRoot>/app/resources/teams`。

### 创建新的 Team

`/NewTeam` 会询问展示名并创建：

```text
<AppRoot>/config/teams/{team_name}/
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

`team.yaml` 示例结构：

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

不内置默认模型。通过环境变量、命令行参数或 `<AppRoot>/config/settings.json` 配置：

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

配置优先级固定为：编译默认值、用户 `config/settings.json`、项目
`.Lumina/CONFIG/defaults.json`、环境变量、CLI 参数。默认路径由 `apppaths`
解析，不再写入用户 settings。

`api-type`：`anthropic`、`openai_compatible`、`auto`。

可以为所有运行链路配置独立的 fallback：

```json
{
  "fallback_api_enabled": true,
  "fallback_api_key": "...",
  "fallback_api_base_url": "https://api.example.com/anthropic",
  "fallback_api_model": "fallback-model",
  "fallback_api_type": "anthropic"
}
```

主模型重试耗尽后，只在 429、5xx、timeout、EOF 和网络错误时切换。无效
Key、错误参数和模型配置错误不会被掩盖；主模型已经输出文本或 tool call
后也不会切换，避免重复输出和重复执行。配置会在下一轮热加载。

`--max-tokens` 是本地上下文窗口长度，用于统计和 80% 压缩阈值。API 请求不会强制携带供应商侧 completion `max_tokens`。runtime 配置会在每轮 Agent 请求前热读取。

## 项目说明文件

读取顺序：

1. `{cwd}/LUMINA.md`
2. `{cwd}/AGENTS.md`
3. `<AppRoot>/config/instructions/LUMINA.md`
4. `<AppRoot>/config/instructions/AGENTS.md`

这些文件都可以不存在。

## Skills

Skill 是包含 `SKILL.md` 的指令包，读取位置：

- `{project_root}/skills/`
- `{project_root}/.Lumina/PROJECT_SKILLS/`
- `<AppRoot>/config/skills/`
- `<AppRoot>/app/resources/skills/`

Skill 上下文会注入本轮模型请求，但不进入可见对话。

```text
/review 检查认证流程有没有安全问题
```

## 工具与权限

工具覆盖文件编辑、Shell、任务、记忆、WebSearch/WebFetch 和 MCP。敏感操作和项目 MCP 可要求人工确认。

## MCP

项目 MCP 配置：`.mcp.json`。信任记录：

```text
<AppRoot>/data/projects/{project-id}/trust/mcp.json
```

使用 `/mcp` 查看已注册 MCP 工具。

## 会话与运行数据

AppRoot 分为五个 ownership layer：

```text
<AppRoot>/
```

- `app/`：可原子替换的程序、前端、内置资源和扩展。
- `config/`：用户设置、MCP、instructions、skills 和 teams。
- `data/`：长期记忆、session、项目 manifest/trust/team 和 legacy 数据。
- `state/`：endpoint、日志、服务、迁移报告和 tool-results。
- `cache/`：模型、下载和可重建临时文件。

项目级 runtime 数据：

```text
<AppRoot>/data/projects/{project-id}/
```

- `project.json`
- `trust/mcp.json`
- `teams/`

项目内用户资源：

- `{project_root}/skills/`
- `{project_root}/.Lumina/PROJECT_SKILLS/`

active session 默认位于 `<AppRoot>/data/sessions/active`，archive 位于
`<AppRoot>/data/sessions/archive`：

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
<AppRoot>/state/projects/{project-id}/tool-results/{session-id}/
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

macOS/Linux 默认 AppRoot 为 `$HOME/.lumina`；Windows 默认
`%LOCALAPPDATA%\LuminaCode`，缺失时回退 `%USERPROFILE%\.lumina`。
`LUMINA_APP_ROOT` 是唯一 root override，`LUMINA_RESOURCE_ROOT` 只覆盖内置
资源。完整协议见 [AppRoot 布局](docs/app-root.md)。

macOS/Linux：

```sh
make install
```

默认安装会先检查本机软硬件、必需工具链、可用空间和推理设备，再从
ModelScope 下载固定 revision 和 SHA-256 的 BGE-M3 画像：Apple Silicon 使用
MLX INT8 与受管 Metal runtime，CPU 使用 ONNX INT8，受支持的受管加速器使用
ONNX FP16。模型、tokenizer、linear heads、原生 runtime 和推理探针全部通过后
才替换已安装应用。BGE-M3 是记忆写入和检索的唯一一个本地模型；模型无效时
安装直接失败，不会回退到另一个向量空间。可用
`LUMINA_MEMORY_EMBEDDING_DEVICE` 显式选择设备，或用
`LUMINA_MEMORY_MODEL_VARIANT=metal-int8|cpu-int8|accelerator-fp16`
固定打包画像。

安装输出会同时写到终端和安装日志。任一阶段中途失败时，安装器会以非零状态
退出，并直接报告失败阶段、原始错误信息、退出码、日志路径和回滚状态；未完成
的 `app.new` 会被清理，已发生的应用交换会自动恢复。

无人值守安装若不允许修改 shell 配置，可使用
`make install NO_PATH_UPDATE=1`；Windows 对应参数为 `-NoPathUpdate`。

Windows：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\install-windows.ps1
```

Doctor：

```sh
make doctor
lumina layout paths --json
lumina layout doctor --json
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

普通卸载会关闭 backend/MCP/SearxNG，删除安装命令和 installer-owned
`app/cache/state`，保留 `config/data/layout.json`、shell rc 和项目内
`.Lumina`。永久删除必须显式执行：

```sh
make purge
# 或：make uninstall PURGE=1
```

```powershell
.\scripts\uninstall-windows.ps1 -Purge
```

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
- `apppaths/`：跨平台 AppRoot、项目身份、doctor 和迁移。
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
