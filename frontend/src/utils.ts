import type blessed from "blessed";

export function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

export function indent(text: string): string {
  return text
    .split("\n")
    .map((line) => `  ${line}`)
    .join("\n");
}

export function formatTokens(value: number): string {
  if (!value) return "0";
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}M`;
  if (value >= 1000) return `${Math.round(value / 1000)}K`;
  return String(value);
}

export function setBox(node: blessed.Widgets.Node, pos: { top: number; left: number; width: number; height: number }): void {
  const box = node as any;
  box.top = pos.top;
  box.left = pos.left;
  box.width = pos.width;
  box.height = pos.height;
}
