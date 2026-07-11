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

- 写入先通过持久化 message cursor 提交原始消息和重叠 evidence chunk；事实、
  实体、时间版本和关系独立 enrichment，daemon 重启后可继续。
- 每次查询固定运行 BM25、本地向量、实体、时间、Session 和图检索。Session
  索引选出相关 Session 后，会在 SQL 层限制 Session 并再次检索 chunk，避免
  关键证据在全局 Top-K 阶段被其他 Session 挤掉。
- 全局结果和 Session 内结果通过等权 RRF、coverage-aware MMR、来源多样性和
  相邻 chunk 扩展统一组装；不增加额外 reranker 模型。
- 每个 Turn 的参考时间会贯穿查询扩展、时间索引、timeline 和隐藏回答上下文。
  canonical entity/event 用于连接跨 Session 的同一对象与事件，同时保留来源。
- user、project、Team、agent type 和 Team agent scope 相互隔离。召回内容是
  临时上下文，不进入可见 transcript。
- 事实同时保留有效时间和观测时间；新事实会替代旧版本，但不会删除历史来源。
- 生命周期将事实有效期与存储保留期分开。记忆按访问和再次确认情况在
  `hot / warm / cold` 间变化；到达保留期、经过宽限期且价值较低时只会归档，
  不会被后台物理删除。pin、有效依赖和未解决冲突会阻止自动归档。

`make install` 会从 ModelScope 安装 `multilingual-e5-small` 到
`~/.lumina/models/memory/`，`make uninstall` 会一并删除。使用 `/Memory`、
`/MemorySearch`、`/MemoryForget`、`/MemoryExport`、`/MemoryImport` 管理记忆。

`~/.lumina/CONFIG/defaults.json` 中常用参数：

| 参数 | 默认值 | 作用 |
|---|---:|---|
| `memory_session_candidates` | 12 | 深入检索的相关 Session 数量 |
| `memory_chunks_per_session` | 6 | 每个 Session 最多保留的融合 chunk |
| `memory_session_chunk_candidates` | 64 | Session 内每个通道的候选数 |
| `memory_adjacent_chunk_window` | 1 | 命中 chunk 两侧附带的相邻窗口 |
| `memory_retrieval_cache_ttl_seconds` | 300 | 按 scope 隔离的召回缓存时间 |
| `memory_query_expansion_timeout_seconds` | 2 | 通用查询扩展的最大等待时间 |
| `memory_lifecycle_enabled` | true | 启用温度、评分和自动归档 |
| `memory_maintenance_interval_seconds` | 300 | 生命周期维护间隔 |
| `memory_hot_access_days` / `memory_warm_access_days` | 30 / 90 | 热、温、冷记忆的访问窗口 |
| `memory_access_recency_half_life_days` | 30 | 访问时效评分的指数衰减半衰期 |
| `memory_archive_grace_days` | 30 | 保留期到期后的归档宽限期 |
| `memory_archive_value_threshold` | 0.45 | 自动归档的最高价值分数 |
| `memory_value_weights` | 见示例配置 | 生命周期价值评分的七项权重 |
| `memory_auto_hard_delete_enabled` | false | 必须保持关闭，禁止后台物理删除 |

四个 `memory_mmr_*_weight` 参数分别控制相关性、新颖性、facet 覆盖和来源覆盖，
总和必须为 `1`。配置会在下一个 Turn 热加载；`make install` 只补充新默认字段，
不会覆盖用户已有值。

生命周期价值分由 importance、confidence、访问时效、访问频率、再次确认、
provenance 和依赖强度组成。`memory_value_weights` 必须完整、非负且总和为 `1`。
`memory_auto_hard_delete_enabled` 只能为 `false`；物理删除必须由用户在 `/Memory`
中明确执行。管理视图可查看 temperature、value score、retention、归档原因和
生命周期事件，并支持 pin、unpin、archive 与 restore。

### LongMemEval

Lumina 在 500 题 oracle 数据集上的成绩为 **72.8%（364/500）**。保存的答案
通过 `https://api.deepseek.com` 使用 LongMemEval 官方判分 prompt 和
`deepseek-v4-pro` 完成 500 题评估。这不是官方 GPT-4o leaderboard 成绩。

| 题型 | 准确率 |
|---|---:|
| Single-session assistant | 98.21% |
| Single-session user | 97.14% |
| Knowledge update | 85.90% |
| Temporal reasoning | 69.92% |
| Single-session preference | 60.00% |
| Multi-session | 47.37% |

本次运行同时把检索质量与答案准确率分开统计：

| 检索指标 | 结果 |
|---|---:|
| Evidence Hit Rate | 86.43% |
| Evidence Recall@K | 73.50% |
| Evidence MRR | 0.443 |
| Source Session Recall | 97.24% |
| Gold Message Recall | 73.99% |
| Injected Chunk Recall | 73.50% |
| Injected Text Coverage | 73.88% |
| 平均记忆上下文 | 1,804 tokens（memory token ratio 70.84%） |

这些指标只统计真正注入回答模型的 evidence chunk。较高的 Session recall 和
相对较低的 chunk recall 表明，当前主要瓶颈是从相关 Session 内选出决定性证据，
尤其是跨 Session 综合。不同报告使用的 judge、reader model、检索深度和上下文
预算不同，横向比较时需要结合评测口径理解。

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
| LiCoMemory | 73.8% | GPT-4o-mini，5 次均值 |
| **LuminaCode** | **72.8%** | DeepSeek Judge，复用官方 prompt；完整 500 题 |
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
