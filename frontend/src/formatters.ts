import type { TuiTheme } from "./theme";
import { indent } from "./utils";

export function escapeBlessedTags(text: string): string {
  return text.replace(/[{}]/g, (char) => (char === "{" ? "{open}" : "{close}"));
}

export function displayAgent(id: string): string {
  if (id === "user") return "你";
  if (id === "team") return "Team";
  if (id === "team-loop") return "Team Loop";
  return id
    .split("-")
    .filter(Boolean)
    .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
    .join(" ");
}

export function appendTeamChatBlock(lines: string[], entry: any, theme: TuiTheme): void {
  const fromID = String(entry?.from_agent || "agent");
  const from = displayAgent(fromID);
  const to = Array.isArray(entry?.to_agent) && entry.to_agent.length ? ` -> ${entry.to_agent.map(displayAgent).join(", ")}` : "";
  const kind = entry?.kind ? ` · ${entry.kind}` : "";
  const headerColor = fromID === "user" ? theme.user : theme.assistant;
  lines.push(`{${headerColor}-fg}{bold}${escapeBlessedTags(from)}${escapeBlessedTags(to)}{/bold}{/${headerColor}-fg}{${theme.muted}-fg}${escapeBlessedTags(kind)}{/${theme.muted}-fg}`);
  const content = String(entry?.content || entry?.summary || "");
  if (content) lines.push(indent(escapeBlessedTags(content)));
  if (Array.isArray(entry?.artifact_refs) && entry.artifact_refs.length) {
    lines.push(indent(`Artifact: ${escapeBlessedTags(entry.artifact_refs.join(", "))}`));
  }
  lines.push("");
}

export function normalizeMenuItems(items: unknown): Array<{ name: string; description: string }> {
  if (!Array.isArray(items)) return [];
  return items.flatMap((item: any) => {
    const rawName = typeof item?.name === "string" ? item.name : typeof item?.Name === "string" ? item.Name : "";
    const name = rawName.trim();
    if (!name) return [];
    const rawDescription = typeof item?.description === "string" ? item.description : typeof item?.Description === "string" ? item.Description : "";
    const description = rawDescription.trim();
    return [{ name, description: escapeBlessedTags(description) }];
  });
}

export function formatPermissionPrompt(payload: any): string {
  const prompt = payload?.prompt ?? payload?.skill_shell_request ?? payload;
  if (typeof prompt === "string") return prompt;
  if (prompt && typeof prompt === "object") {
    const request = (prompt as any).skill_shell_request || (prompt as any).input || prompt;
    const parts: string[] = [];
    if (request.agent_display || payload?.agent_display) parts.push(`Agent: ${request.agent_display || payload.agent_display}`);
    const toolName = request.tool_name || request.tool || request.name || request.skill || request.skill_name;
    if (toolName) parts.push(`Tool: ${toolName}`);
    if (request.risk) parts.push(`Risk: ${request.risk}`);
    if (request.command || request.cmd) parts.push(`Command:\n${request.command || request.cmd}`);
    if (request.file_path || request.path) parts.push(`Path: ${request.file_path || request.path}`);
    if (request.cwd || request.workdir) parts.push(`CWD: ${request.cwd || request.workdir}`);
    if (request.reason || request.description) parts.push(`Reason:\n${request.reason || request.description}`);
    if (request.summary) parts.push(`Summary:\n${request.summary}`);
    if (parts.length > 0) return parts.join("\n");
    return "该工具请求需要权限确认。";
  }
  return "tool permission";
}
