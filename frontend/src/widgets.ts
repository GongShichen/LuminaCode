import blessed from "blessed";

import type { TuiTheme } from "./theme";

export function createTuiWidgets(theme: TuiTheme) {
  const screen = blessed.screen({
    smartCSR: true,
    fullUnicode: true,
    mouse: true,
    terminal: process.env.TERM && process.env.TERM !== "dumb" ? process.env.TERM : "xterm-256color",
    title: "LuminaCode",
    useBCE: false,
    style: { fg: theme.text, bg: theme.background },
  });

  const header = blessed.box({
    top: 0,
    left: 0,
    width: "100%",
    height: 1,
    tags: true,
    content: `{${theme.brand}-fg}{bold}LuminaCode{/bold}{/${theme.brand}-fg}`,
  });

  const transcript = blessed.box({
    label: " 对话记录 ",
    tags: true,
    top: 1,
    left: 1,
    width: "100%-2",
    height: "54%",
    border: "line",
    mouse: false,
    keys: false,
    transparent: false,
    scrollable: true,
    alwaysScroll: true,
    scrollbar: { ch: " ", track: { bg: "black" }, style: { bg: "cyan" } },
    padding: { left: 1, right: 1 },
    style: { fg: theme.text, bg: theme.panelBg, border: { fg: theme.panelBorder }, label: { fg: theme.panelLabel } },
  });

  const tasks = blessed.box({
    label: " 任务概览 ",
    top: "55%",
    left: 1,
    width: "100%-2",
    height: "18%",
    border: "line",
    mouse: false,
    keys: false,
    transparent: false,
    scrollable: true,
    alwaysScroll: true,
    scrollbar: { ch: " ", track: { bg: "black" }, style: { bg: "blue" } },
    padding: { left: 1, right: 1 },
    style: { fg: theme.text, bg: theme.panelBg, border: { fg: theme.panelBorder }, label: { fg: theme.panelLabel } },
  });

  const status = blessed.box({
    label: " 状态 ",
    bottom: 5,
    left: 1,
    width: "100%-2",
    height: 3,
    border: "line",
    transparent: false,
    padding: { left: 1, right: 1 },
    style: { fg: theme.text, bg: theme.panelBg, border: { fg: theme.subtleBorder }, label: { fg: theme.muted } },
  });

  const input = blessed.box({
    label: " ● 输入 ",
    bottom: 0,
    left: 1,
    width: "100%-2",
    height: 5,
    border: "line",
    mouse: false,
    keys: false,
    scrollable: true,
    alwaysScroll: true,
    transparent: false,
    padding: { left: 1, right: 1 },
    scrollbar: { ch: " ", track: { bg: "black" }, style: { bg: "green" } },
    style: { fg: theme.text, bg: theme.panelBg, border: { fg: theme.inputBorder }, label: { fg: theme.inputBorder } },
  });

  const menu = blessed.list({
    hidden: true,
    mouse: true,
    keys: false,
    top: "70%",
    left: 6,
    width: "70%",
    height: 10,
    border: "line",
    transparent: false,
    padding: { left: 1, right: 1 },
    style: { fg: theme.text, bg: theme.panelBg, selected: { bg: theme.selectionBg, fg: theme.selectionFg }, border: { fg: theme.inputBorder } },
  });

  const modal = blessed.box({
    hidden: true,
    top: "center",
    left: "center",
    width: "70%",
    height: "50%",
    border: "line",
    tags: true,
    mouse: false,
    keys: false,
    scrollable: true,
    alwaysScroll: true,
    transparent: false,
    padding: { left: 1, right: 1 },
    scrollbar: { ch: " ", track: { bg: "black" }, style: { bg: "yellow" } },
    style: { fg: theme.text, bg: theme.panelBg, border: { fg: theme.warning } },
  });

  return { screen, header, transcript, tasks, status, input, menu, modal };
}

export type TuiWidgets = ReturnType<typeof createTuiWidgets>;
