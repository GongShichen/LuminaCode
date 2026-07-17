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

## 长期记忆

跨 Session 记忆保存在本地 SQLite：

```text
~/.lumina/memory/lumina-memory.sqlite
```

### 1、记什么，怎么记

- Lumina 会保存可见对话、有价值的工具观察、用户偏好、项目决策、可复用流程、
  事实、实体、事件及其关系；隐藏 reasoning、凭据、权限 payload 和重复 tool dump
  不进入长期记忆。
- 原始消息会先作为不可变 evidence 提交，再按较大的 chunk 和较小的 evidence
  atom 组织。Atom 能识别句子、列表项和代码块，同时保留 Session、角色、时间、
  原文位置、provenance 和访问 scope，因此任何记忆都能追溯到原始上下文。
- 可恢复的后台任务进一步生成结构化 fact、entity、relation、event、preference
  和本地 E5 embedding。事实更新不会覆盖历史，而是保留有效时间、观测时间、
  冲突和 superseded 版本。
- user、project、Team、agent type 和 Team agent 记忆按 scope 隔离。常用或再次
  确认的记忆保持 hot；过期且低价值的记录可以归档，但后台维护不会物理删除。

可通过 `/Memory`、`/MemorySearch`、`/MemoryForget`、`/MemoryExport` 和 `/MemoryImport` 查看与管理。

### 2、找什么，用什么方式找

- 每次查询都会固定搜索原词、语义相似内容、实体、时间、Session 和关系，对应
  BM25、向量、实体、时间、Session 和图六个通道。模型生成的通用查询扩展只能
  补充同义表达和结构化提示，不能关闭通道、修改 scope 或排除某类记忆。
- 六类独立信号融合后，Evidence Ledger 选择能够覆盖不同信息需求、可追溯到原文
  的小粒度 atom。证据不足时再执行一轮全通道补充检索。最终只把选中的证据、
  必要的局部结构、provenance 和 timeline 作为一条临时隐藏上下文交给主模型，
  不重复注入 Session 摘要或完整 transcript。

Lumina 在 500 题 LongMemEval oracle 数据集上的成绩为 **86.0%（430/500）**。
评估通过 `https://api.deepseek.com` 使用 LongMemEval 官方判分 prompt 和
`deepseek-v4-pro` 完成。这是生产记忆链路的黑盒测试，不是官方 GPT-4o
leaderboard 成绩。

| 题型 | 准确率 |
|---|---:|
| Single-session user | 97.14% |
| Knowledge update | 91.03% |
| Temporal reasoning | 88.72% |
| Single-session preference | 80.00% |
| Multi-session | 79.70% |
| Single-session assistant | 76.79% |

本次运行同时把检索质量与答案准确率分开统计：

| 检索指标 | 结果 |
|---|---:|
| Evidence Hit Rate | 99.79% |
| Evidence Recall@K | 95.75% |
| Evidence MRR | 0.701 |
| Source Session Recall | 100.00% |
| Gold Message Recall | 98.05% |
| Injected Chunk Recall | 95.75% |
| Injected Text Coverage | 88.13% |
| 平均记忆上下文 | 1,717 tokens（memory token ratio 22.59%） |
| 平均检索耗时 | 8.34 秒 |

检索指标根据真正注入回答模型的 evidence atom ID 和原始 source span 计算。

#### 完整记忆库 LongMemEval-S

上面的 oracle 成绩属于检索上限设置：每题只提供能够支持答案的 Session。
清洗后的 LongMemEval-S 使用相同的 500 个 question ID，但提供完整对话记忆库。
每题平均搜索空间因此从 1.90 个 Session、21.92 条消息，增加到 47.73 个
Session、493.50 条消息。

在同样使用 `mimo-v2.5-pro` 回答模型和 `deepseek-v4-pro` 官方判分 prompt 的
情况下，Lumina 在 LongMemEval-S 上取得 **75.8%（379/500）**：

| 题型 | Oracle | LongMemEval-S | 差值 |
|---|---:|---:|---:|
| Overall | 86.00%（430/500） | 75.80%（379/500） | -10.20 pp |
| Single-session user | 97.14% | 88.57% | -8.57 pp |
| Knowledge update | 91.03% | 89.74% | -1.29 pp |
| Temporal reasoning | 88.72% | 80.45% | -8.27 pp |
| Single-session preference | 80.00% | 53.33% | -26.67 pp |
| Multi-session | 79.70% | 63.91% | -15.79 pp |
| Single-session assistant | 76.79% | 69.64% | -7.15 pp |

检索指标能够解释大部分答案准确率差距：

| 检索指标 | Oracle | LongMemEval-S |
|---|---:|---:|
| Evidence Hit Rate | 99.79% | 91.44% |
| Gold Message Recall | 98.05% | 84.95% |
| Injected Chunk Recall | 95.75% | 80.75% |
| Injected Text Coverage | 88.13% | 71.20% |
| Source Session Recall | 100.00% | 98.44% |
| Evidence MRR | 0.701 | 0.504 |
| 平均召回 evidence | 27.95 | 36.86 |
| 平均记忆上下文 | 1,717 tokens | 2,344 tokens |
| 平均检索耗时 | 8.34 秒 | 9.48 秒 |

在 479 个带 evidence 标注的问题中，完整召回全部 gold message 的题目从
oracle 的 456 题降到 LongMemEval-S 的 373 题。当 gold message 全部可见时，
答案准确率几乎没有变化：oracle 为 88.16%，LongMemEval-S 为 87.67%。因此，
整体 10.2 个百分点的差距主要来自完整记忆库中精确证据漏召回、只召回部分证据
或证据排名过低，而不是回答模型能力发生了系统性回归。本次 LongMemEval-S
运行完成了 500 条唯一 prediction，运行和检索通道错误均为 0；75.8% 是当前
完整记忆库 baseline，不应与 oracle-only 成绩直接比较。

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
| **LuminaCode（LongMemEval-S）** | **75.8%** | 完整 haystack；DeepSeek Judge；复用官方 prompt；完整 500 题 |
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
[Exabase M-1 公告](https://www.prnewswire.com/news-releases/exabase-achieves-highest-reported-score-on-leading-ai-memory-benchmark-using-a-smaller-cheaper-model-302780919.html)。Exabase 和 Honcho
使用公开报告分数，但完整复现材料相对有限。不同报告的 reader、检索深度、
上下文预算和 judge 不完全一致，因此这不是严格同口径排行榜。

公开 LoCoMo LLM-Judge 排名：

| 系统 | 总分 | Multi-Hop | Temporal | Open Domain | Single-Hop |
|---|---:|---:|---:|---:|---:|
| [Attemory](https://github.com/AttemorySystem/attemory/blob/main/benchmarks/results/LoCoMo/report.txt) | 94.52% | 81.25% | 92.52% | 96.91% | 93.97% |
| [MemoryLake](https://github.com/memorylake-ai/memorylake-locomo-benchmark) | 94.03% | 91.84% | 91.28% | 85.42% | 96.79% |
| [EverMemOS](https://arxiv.org/abs/2601.02163) | 93.05% | 91.84% | 89.72% | 76.04% | 96.67% |
| [MemCog](https://arxiv.org/abs/2605.28046) | 92.98% | 80.21% | 92.81% | 94.89% | 91.84% |
| [Backboard](https://github.com/Backboard-io/Backboard-Locomo-Benchmark) | 90.00% | 75.00% | 91.90% | 91.20% | 89.36% |
| [Hindsight](https://github.com/vectorize-io/hindsight-benchmarks) | 89.61% | 70.83% | 83.80% | 95.12% | 86.17% |
| **LuminaCode** | **77.40%** | **55.21%** | **76.32%** | **82.05%** | **72.34%** |
| [Memobase v0.0.37](https://github.com/memodb-io/memobase/blob/main/docs/experiments/locomo-benchmark/README.md) | 75.78% | 46.88% | 85.05% | 77.17% | 70.92% |
| Zep | 75.14% | 66.04% | 79.79% | 67.71% | 74.11% |
| Mem0-Graph | 68.44% | 47.19% | 58.13% | 75.71% | 65.71% |
| Mem0 | 66.88% | 51.15% | 55.51% | 72.93% | 67.13% |

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
