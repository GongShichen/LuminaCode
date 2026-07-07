import { appendTeamChatBlock, escapeBlessedTags } from "./formatters";
import type { TuiTheme } from "./theme";
import type { TranscriptEntry } from "./types";
import { formatTokens, indent } from "./utils";

export function buildTranscriptContent(args: {
  teamMode: boolean;
  transcriptEntries: TranscriptEntry[];
  teamDialogueEntries: any[];
  teamStreamingText: Map<string, string>;
  theme: TuiTheme;
}): string {
  const lines: string[] = [];
  if (args.teamMode) {
    for (const entry of args.teamDialogueEntries) {
      appendTeamChatBlock(lines, entry, args.theme);
    }
    for (const [agentID, text] of args.teamStreamingText.entries()) {
      if (!text.trim()) continue;
      appendTeamChatBlock(
        lines,
        {
          from_agent: agentID,
          to_agent: ["team"],
          kind: "streaming",
          content: text,
        },
        args.theme,
      );
    }
    return lines.join("\n");
  }

  for (const entry of args.transcriptEntries) {
    if (entry.kind === "user") {
      lines.push(`{${args.theme.user}-fg}{bold}你{/bold}{/${args.theme.user}-fg}`);
      lines.push(indent(escapeBlessedTags(entry.text)));
    } else {
      lines.push(`{${args.theme.assistant}-fg}{bold}Lumina{/bold}{/${args.theme.assistant}-fg}`);
      lines.push(indent(escapeBlessedTags(entry.text)));
    }
    lines.push("");
  }
  return lines.join("\n");
}

export function buildTasksContent(args: {
  teamMode: boolean;
  running: boolean;
  spinnerFrame: string;
  spinnerDots: string;
  teamLoopIteration: number;
  teamActivityRows: any[];
  teamArtifacts: any[];
  teamContract: any;
  teamGateVerdicts: any;
  taskLines: string[];
}): string {
  if (args.teamMode) {
    const active = args.running ? `${args.spinnerFrame} Team Loop #${args.teamLoopIteration} running${args.spinnerDots}` : "Team idle";
    const lines = [active];
    const contractState = args.teamContract ? `recorded  ${args.teamContract.project_root || ""}`.trim() : "missing";
    lines.push(`Contract      ${contractState}`);
    const qa = args.teamGateVerdicts?.qa?.status || "pending";
    const reviewer = args.teamGateVerdicts?.reviewer?.status || "pending";
    const blocking = countBlockingFindings(args.teamGateVerdicts);
    lines.push(`Gates         QA ${qa} | Reviewer ${reviewer}${blocking > 0 ? ` | blocking ${blocking}` : ""}`);
    for (const row of args.teamActivityRows) {
      const name = row.display_name || row.agent_id || "agent";
      lines.push(`${name.padEnd(14)} ${String(row.status || "idle").padEnd(12)} ${row.summary || ""}`);
    }
    if (args.teamArtifacts.length) {
      lines.push(`Artifacts     ${args.teamArtifacts.length}          ${args.teamArtifacts.map((a) => a.name || a.id).slice(0, 6).join(", ")}`);
    }
    return lines.join("\n");
  }

  const prefix = args.running ? `${args.spinnerFrame} 正在执行任务${args.spinnerDots}` : "空闲";
  return (args.taskLines.length ? args.taskLines : [prefix]).join("\n");
}

export function buildHeaderContent(args: {
  teamMode: boolean;
  activeTeamName: string;
  running: boolean;
  spinnerFrame: string;
  theme: TuiTheme;
}): string {
  const state = args.running ? `${args.spinnerFrame} ${args.teamMode ? "Team is working" : "Agent is thinking"}` : "输入就绪";
  const title = args.teamMode ? `LuminaCode · Team: ${escapeBlessedTags(args.activeTeamName || "Team")}` : "LuminaCode";
  return `{${args.theme.brand}-fg}{bold}${title}{/bold}{/${args.theme.brand}-fg}{|}{${args.theme.muted}-fg}${state}{/${args.theme.muted}-fg}`;
}

export function buildStatusContent(args: {
  teamMode: boolean;
  activeTeamName: string;
  teamLoopIteration: number;
  teamActivityRows: any[];
  teamGateStatus: any;
  teamContract: any;
  teamGateVerdicts: any;
  frame: any;
}): string {
  if (args.teamMode) {
    const active = args.teamActivityRows.filter((row) => row.status === "running").map((row) => row.display_name || row.agent_id).join(", ") || "none";
    const qa = args.teamGateVerdicts?.qa?.status || args.teamGateStatus?.qa || "pending";
    const reviewer = args.teamGateVerdicts?.reviewer?.status || args.teamGateStatus?.reviewer || "pending";
    const contract = args.teamContract ? "contract recorded" : "contract missing";
    const blocking = countBlockingFindings(args.teamGateVerdicts);
    const gate = `QA ${qa} / Reviewer ${reviewer}${blocking > 0 ? ` / blocking ${blocking}` : ""}`;
    return `Team: ${args.activeTeamName || "Team"} | Loop #${args.teamLoopIteration} | ${contract} | Active: ${active} | Gate: ${gate}`;
  }

  const model = args.frame?.model_name || args.frame?.model || "unknown";
  const used = Number(args.frame?.context_used_tokens || 0);
  const limit = Number(args.frame?.context_limit_tokens || 0);
  const ratio = limit > 0 ? Math.min(1, used / limit) : 0;
  const barWidth = 24;
  const filled = Math.round(ratio * barWidth);
  const bar = `${"=".repeat(filled)}${"-".repeat(barWidth - filled)}`;
  return `Model: ${model} | Context [${bar}] ${Math.round(ratio * 100)}% ${formatTokens(used)}/${formatTokens(limit)}`;
}

function countBlockingFindings(verdicts: any): number {
  let count = 0;
  for (const verdict of Object.values(verdicts || {}) as any[]) {
    for (const finding of verdict?.findings || []) {
      if (finding?.blocking) count += 1;
    }
  }
  return count;
}
