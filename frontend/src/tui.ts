import blessed from "blessed";
import { execFileSync } from "node:child_process";

import { RpcClient } from "./rpc";
import type { LaunchOptions, PushEvent, TranscriptEntry } from "./types";
import { formatTokens, indent, setBox } from "./utils";

const spinnerFrames = ["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"];
const escInterruptWindowMs = 800;
const tuiTheme = createTheme();

export class LuminaTui {
  private screen = blessed.screen({
    smartCSR: true,
    fullUnicode: true,
    terminal: process.env.TERM && process.env.TERM !== "dumb" ? process.env.TERM : "xterm-256color",
    title: "LuminaCode",
    useBCE: false,
    style: { fg: tuiTheme.text, bg: tuiTheme.background },
  });
  private header = blessed.box({ top: 0, left: 0, width: "100%", height: 1, tags: true, content: `{${tuiTheme.brand}-fg}{bold}LuminaCode{/bold}{/${tuiTheme.brand}-fg}` });
  private transcript = blessed.box({
    label: " 对话记录 ",
    tags: true,
    top: 1,
    left: 1,
    width: "100%-2",
    height: "54%",
    border: "line",
    mouse: true,
    keys: false,
    transparent: false,
    scrollable: true,
    alwaysScroll: true,
    scrollbar: { ch: " ", track: { bg: "black" }, style: { bg: "cyan" } },
    padding: { left: 1, right: 1 },
    style: { fg: tuiTheme.text, bg: tuiTheme.panelBg, border: { fg: tuiTheme.panelBorder }, label: { fg: tuiTheme.panelLabel } },
  });
  private tasks = blessed.box({
    label: " 任务概览 ",
    top: "55%",
    left: 1,
    width: "100%-2",
    height: "18%",
    border: "line",
    mouse: true,
    keys: false,
    transparent: false,
    scrollable: true,
    alwaysScroll: true,
    scrollbar: { ch: " ", track: { bg: "black" }, style: { bg: "blue" } },
    padding: { left: 1, right: 1 },
    style: { fg: tuiTheme.text, bg: tuiTheme.panelBg, border: { fg: tuiTheme.panelBorder }, label: { fg: tuiTheme.panelLabel } },
  });
  private status = blessed.box({
    label: " 状态 ",
    bottom: 5,
    left: 1,
    width: "100%-2",
    height: 3,
    border: "line",
    transparent: false,
    padding: { left: 1, right: 1 },
    style: { fg: tuiTheme.text, bg: tuiTheme.panelBg, border: { fg: tuiTheme.subtleBorder }, label: { fg: tuiTheme.muted } },
  });
  private input = blessed.box({
    label: " ● 输入 ",
    bottom: 0,
    left: 1,
    width: "100%-2",
    height: 5,
    border: "line",
    mouse: true,
    keys: false,
    transparent: false,
    padding: { left: 1, right: 1 },
    style: { fg: tuiTheme.text, bg: tuiTheme.panelBg, border: { fg: tuiTheme.inputBorder }, label: { fg: tuiTheme.inputBorder } },
  });
  private menu = blessed.list({
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
    style: { fg: tuiTheme.text, bg: tuiTheme.panelBg, selected: { bg: tuiTheme.selectionBg, fg: tuiTheme.selectionFg }, border: { fg: tuiTheme.inputBorder } },
  });
  private modal = blessed.box({
    hidden: true,
    top: "center",
    left: "center",
    width: "70%",
    height: 9,
    border: "line",
    tags: true,
    transparent: false,
    padding: { left: 1, right: 1 },
    style: { fg: tuiTheme.text, bg: tuiTheme.panelBg, border: { fg: tuiTheme.warning } },
  });

  private sessionID = "";
  private transcriptEntries: TranscriptEntry[] = [];
  private taskLines: string[] = [];
  private slashItems: Array<{ name: string; description: string }> = [];
  private running = false;
  private spinner = 0;
  private lastFrame: any = {};
  private history: string[] = [];
  private historyIndex = -1;
  private historyDraft = "";
  private menuMode: "slash" | "skill" | "resume" | null = null;
  private inputBuffer = "";
  private inputCursor = 0;
  private inputEnabled = true;
  private inputPlaceholder = "请输入消息并回车。";
  private inputDataHandler?: (chunk: Buffer | string) => void;
  private transcriptFollow = true;
  private tasksFollow = true;
  private suppressTranscriptScroll = false;
  private suppressTasksScroll = false;
  private lastEscapeAt = 0;
  private escapeResetTimer?: NodeJS.Timeout;

  constructor(private rpc: RpcClient, private options: LaunchOptions) {}

  async start(): Promise<void> {
    this.screen.append(this.header);
    this.screen.append(this.transcript);
    this.screen.append(this.tasks);
    this.screen.append(this.status);
    this.screen.append(this.input);
    this.screen.append(this.menu);
    this.screen.append(this.modal);
    this.layout();
    this.bindKeys();
    this.prepareTerminalInput();
    this.rpc.onEvent((event) => this.handlePush(event));
    const snapshot = this.options.resumeSessionID
      ? await this.rpc.call("session.resume", { session_id: this.options.resumeSessionID, cwd: this.options.cwd })
      : await this.rpc.call("session.create", { cwd: this.options.cwd });
    this.applySnapshot(snapshot);
    await this.loadSlashItems();
    this.input.focus();
    this.renderInput();
    setInterval(() => {
      if (!this.running) return;
      this.spinner = (this.spinner + 1) % 4;
      this.renderStatus();
      this.renderTasks();
      this.screen.render();
    }, 140);
    this.screen.render();
  }

  private bindKeys(): void {
    this.screen.on("resize", () => {
      this.layout();
      this.screen.render();
    });
    this.transcript.on("wheelup", () => {
      this.transcriptFollow = false;
    });
    this.transcript.on("wheeldown", () => {
      this.transcriptFollow = isPaneAtBottom(this.transcript);
    });
    this.transcript.on("scroll", () => {
      if (!this.suppressTranscriptScroll) {
        this.transcriptFollow = isPaneAtBottom(this.transcript);
      }
    });
    this.tasks.on("wheelup", () => {
      this.tasksFollow = false;
    });
    this.tasks.on("wheeldown", () => {
      this.tasksFollow = isPaneAtBottom(this.tasks);
    });
    this.tasks.on("scroll", () => {
      if (!this.suppressTasksScroll) {
        this.tasksFollow = isPaneAtBottom(this.tasks);
      }
    });
  }

  private prepareTerminalInput(): void {
    const input = (this.screen.program as any)?.input || process.stdin;
    input.setEncoding?.("utf8");
    input.resume?.();
    if (input.isTTY && typeof input.setRawMode === "function") {
      input.setRawMode(true);
    }
    this.inputDataHandler = (chunk: Buffer | string) => {
      void this.handleRawInput(String(chunk));
    };
    input.on?.("data", this.inputDataHandler);
    process.once("exit", () => {
      if (this.inputDataHandler) {
        input.off?.("data", this.inputDataHandler);
      }
      if (input.isTTY && typeof input.setRawMode === "function") {
        input.setRawMode(false);
      }
    });
  }

  private exit(): void {
    const input = (this.screen.program as any)?.input || process.stdin;
    if (this.inputDataHandler) {
      input.off?.("data", this.inputDataHandler);
      this.inputDataHandler = undefined;
    }
    if (input.isTTY && typeof input.setRawMode === "function") {
      input.setRawMode(false);
    }
    process.exit(0);
  }

  private async exitFromCommand(): Promise<void> {
    try {
      await this.rpc.call("session.exit", { session_id: this.sessionID });
    } catch {
      // Exit should still work if the backend is already gone.
    }
    this.exit();
  }

  private async handleRawInput(text: string): Promise<void> {
    let i = 0;
    while (i < text.length) {
      const rest = text.slice(i);
      const terminalEvent = parseTerminalControlSequence(rest);
      if (terminalEvent) {
        i += terminalEvent.length;
        continue;
      }
      const cursor = parseCursorSequence(rest);
      if (cursor) {
        await this.handleInputKey(undefined, { name: cursor.name });
        i += cursor.length;
        continue;
      }
      if (rest.startsWith("\x1b[3~")) {
        await this.handleInputKey(undefined, { name: "delete" });
        i += 4;
        continue;
      }
      if (rest.startsWith("\x1b[A")) {
        await this.handleInputKey(undefined, { name: "up" });
        i += 3;
        continue;
      }
      if (rest.startsWith("\x1b[B")) {
        await this.handleInputKey(undefined, { name: "down" });
        i += 3;
        continue;
      }
      if (rest.startsWith("\x1b[C")) {
        await this.handleInputKey(undefined, { name: "right" });
        i += 3;
        continue;
      }
      if (rest.startsWith("\x1b[D")) {
        await this.handleInputKey(undefined, { name: "left" });
        i += 3;
        continue;
      }
      if (rest.startsWith("\x1b[H") || rest.startsWith("\x1b[1~")) {
        await this.handleInputKey(undefined, { name: "home" });
        i += rest.startsWith("\x1b[1~") ? 4 : 3;
        continue;
      }
      if (rest.startsWith("\x1b[F") || rest.startsWith("\x1b[4~")) {
        await this.handleInputKey(undefined, { name: "end" });
        i += rest.startsWith("\x1b[4~") ? 4 : 3;
        continue;
      }
      const char = Array.from(rest)[0] || "";
      i += char.length || 1;
      switch (char) {
        case "\x03":
          this.exit();
          return;
        case "\r":
        case "\n":
          await this.handleInputKey(undefined, { name: "enter" });
          break;
        case "\t":
          await this.handleInputKey(undefined, { name: "tab" });
          break;
        case "\x1b":
          await this.handleInputKey(undefined, { name: "escape" });
          break;
        case "\x7f":
        case "\b":
          await this.handleInputKey(undefined, { name: "backspace" });
          break;
        case "\x01":
          await this.handleInputKey(undefined, { name: "C-a" });
          break;
        case "\x05":
          await this.handleInputKey(undefined, { name: "C-e" });
          break;
        case "\x15":
          await this.handleInputKey(undefined, { name: "C-u" });
          break;
        default:
          await this.handleInputKey(char, { name: "char" });
          break;
      }
    }
  }

  private async handleInputKey(ch: string | undefined, key: any): Promise<void> {
    if (!this.modal.hidden) return;
    const name = key?.full || key?.name || "";
    if (!this.menu.hidden && this.menuMode) {
      switch (name) {
        case "up":
          this.menu.up(1);
          this.screen.render();
          return;
        case "down":
          this.menu.down(1);
          this.screen.render();
          return;
        case "enter":
        case "return":
          if (this.menuMode === "slash" && this.inputBuffer.trim() === this.selectedMenuToken()) {
            this.hideMenu();
            await this.submitInput();
            return;
          }
          await this.acceptMenuSelection();
          return;
        case "tab":
          await this.completeMenuSelection();
          return;
        case "escape":
          this.hideMenu();
          return;
        default:
          if (this.menuMode !== "slash") return;
      }
    }
    switch (name) {
      case "enter":
      case "return":
        await this.submitInput();
        return;
      case "tab":
        this.updateCompletion();
        if (!this.menu.hidden && this.menuMode) await this.completeMenuSelection();
        return;
      case "escape":
        await this.handleEscape();
        return;
      case "up":
        this.historyUp();
        return;
      case "down":
        this.historyDown();
        return;
      case "left":
        this.moveInputCursor(-1);
        return;
      case "right":
        this.moveInputCursor(1);
        return;
      case "home":
      case "C-a":
        this.inputCursor = 0;
        this.renderInput();
        this.screen.render();
        return;
      case "end":
      case "C-e":
        this.inputCursor = Array.from(this.inputBuffer).length;
        this.renderInput();
        this.screen.render();
        return;
      case "backspace":
        this.deleteBeforeCursor();
        return;
      case "delete":
        this.deleteAtCursor();
        return;
      case "C-u":
        this.setInput("");
        return;
    }
    if (!this.inputEnabled || this.running) return;
    if (isPrintableInput(ch)) {
      this.insertInputText(ch || "");
    }
  }

  private async submitInput(): Promise<void> {
    const text = this.inputBuffer.trim();
    if (!text) return;
    if (text === "/quit" || text === "/exit" || text === "/q") {
      await this.exitFromCommand();
      return;
    }
    if (text === "/clear") {
      const snapshot = await this.rpc.call("session.clear", { session_id: this.sessionID });
      this.applySnapshot(snapshot);
      this.setInput("");
      return;
    }
    if (text === "/tokens") {
      const tokens = await this.rpc.call("session.tokens", { session_id: this.sessionID });
      this.taskLines.push(`tokens: ${tokens.total_tokens} total (${tokens.input_tokens} in / ${tokens.output_tokens} out)`);
      this.renderTasks();
      this.setInput("");
      return;
    }
    if (text === "/yolo") {
      const result = await this.rpc.call("session.yolo", { session_id: this.sessionID });
      this.taskLines.push(`yolo: ${result.yolo ? "enabled" : "disabled"}`);
      this.renderTasks();
      this.setInput("");
      return;
    }
    if (text === "/save" || text === "/s") {
      await this.rpc.call("session.save", { session_id: this.sessionID });
      this.taskLines.push("session saved");
      this.renderTasks();
      this.setInput("");
      return;
    }
    if (text === "/compact" || text === "/compress") {
      await this.rpc.call("session.compact", { session_id: this.sessionID });
      this.setInput("");
      return;
    }
    if (text === "/skill") {
      await this.showSkills();
      return;
    }
    if (text === "/resume") {
      await this.showSessions();
      return;
    }
    this.history.unshift(text);
    this.resetEscapeInterrupt();
    this.historyIndex = -1;
    this.historyDraft = "";
    this.hideMenu();
    this.setInput("");
    this.running = true;
    this.transcriptFollow = true;
    this.tasksFollow = true;
    this.inputEnabled = false;
    this.renderStatus();
    this.renderInput();
    try {
      await this.rpc.call("session.submit", { session_id: this.sessionID, input: text });
    } catch (err) {
      this.taskLines.push(String(err));
      this.running = false;
      this.inputEnabled = true;
      this.renderTasks();
      this.renderStatus();
      this.renderInput();
    }
  }

  private async handleEscape(): Promise<void> {
    if (!this.running) {
      this.hideMenu();
      this.resetEscapeInterrupt();
      return;
    }
    if (this.inputBuffer.trim() !== "") {
      this.resetEscapeInterrupt();
      return;
    }
    const now = Date.now();
    if (now - this.lastEscapeAt <= escInterruptWindowMs) {
      this.resetEscapeInterrupt();
      this.inputPlaceholder = "正在中断当前会话...";
      this.renderInput();
      this.screen.render();
      try {
        await this.rpc.call("session.abort", { session_id: this.sessionID });
      } catch (err) {
        this.taskLines.push(`abort: ${String(err)}`);
        this.renderTasks();
      }
      return;
    }
    this.lastEscapeAt = now;
    this.inputPlaceholder = "再按一次 Esc 中断当前会话。";
    this.renderInput();
    this.screen.render();
    if (this.escapeResetTimer) clearTimeout(this.escapeResetTimer);
    this.escapeResetTimer = setTimeout(() => {
      this.lastEscapeAt = 0;
      if (this.running) {
        this.inputPlaceholder = "Agent is responding...";
        this.renderInput();
        this.screen.render();
      }
    }, escInterruptWindowMs);
  }

  private resetEscapeInterrupt(): void {
    this.lastEscapeAt = 0;
    if (this.escapeResetTimer) {
      clearTimeout(this.escapeResetTimer);
      this.escapeResetTimer = undefined;
    }
  }

  private async loadSlashItems(): Promise<void> {
    const data = await this.rpc.call("slash.list", { session_id: this.sessionID });
    this.slashItems = normalizeMenuItems(data.items);
  }

  private updateCompletion(): void {
    const value = this.inputBuffer;
    if (!value.startsWith("/") || value.includes(" ")) {
      this.hideMenu();
      return;
    }
    const matches = this.slashItems.filter((item) => item.name.startsWith(value)).slice(0, 10);
    if (matches.length === 0) {
      this.hideMenu();
      return;
    }
    this.menuMode = "slash";
    this.menu.setItems(matches.map((item) => `${item.name.padEnd(18)} ${item.description || ""}`));
    this.menu.select(0);
    this.menu.show();
    this.screen.render();
  }

  private async acceptMenuSelection(): Promise<void> {
    const index = Number((this.menu as any).selected || 0);
    if (this.menuMode === "skill") {
      const item = this.menu.getItem(index)?.getText().trim().split(/\s+/)[0];
      if (item) this.setInput(`/${item} `);
      this.hideMenu();
      return;
    }
    if (this.menuMode === "resume") {
      const sessionID = this.menu.getItem(index)?.getText().trim().split(/\s+/)[0];
      if (sessionID) {
        const snapshot = await this.rpc.call("session.resume", { session_id: sessionID, cwd: this.options.cwd });
        this.applySnapshot(snapshot);
        await this.loadSlashItems();
      }
      this.hideMenu();
      this.setInput("");
      return;
    }
    const selected = this.menu.getItem(index)?.getText().trim().split(/\s+/)[0];
    if (!selected) return;
    if (selected === "/skill") {
      await this.showSkills();
      return;
    }
    if (selected === "/resume") {
      await this.showSessions();
      return;
    }
    this.setInput(`${selected} `);
    this.hideMenu();
  }

  private async completeMenuSelection(): Promise<void> {
    const index = Number((this.menu as any).selected || 0);
    if (this.menuMode === "slash") {
      const selected = this.selectedMenuToken();
      if (selected) {
        this.setInput(selected);
        this.hideMenu();
      }
      return;
    }
    await this.acceptMenuSelection();
  }

  private selectedMenuToken(): string {
    const index = Number((this.menu as any).selected || 0);
    return this.menu.getItem(index)?.getText().trim().split(/\s+/)[0] || "";
  }

  private async showSkills(): Promise<void> {
    const skills = await this.rpc.call("skills.list", { session_id: this.sessionID });
    this.menuMode = "skill";
    this.menu.setItems(normalizeMenuItems(skills).map((skill) => `${skill.name.padEnd(24)} ${skill.description}`));
    this.menu.select(0);
    this.menu.show();
    this.menu.focus();
    this.screen.render();
  }

  private async showSessions(): Promise<void> {
    const sessions = await this.rpc.call("session.list");
    this.menuMode = "resume";
    this.menu.setItems((Array.isArray(sessions) ? sessions : []).flatMap((s: any) => {
      const sessionID = typeof s?.session_id === "string" ? s.session_id : "";
      if (!sessionID) return [];
      return [`${sessionID}  turns:${s.turn_count || 0}  messages:${s.message_count || 0}`];
    }));
    this.menu.select(0);
    this.menu.show();
    this.menu.focus();
    this.screen.render();
  }

  private handlePush(push: PushEvent): void {
    if (push.session_id && push.session_id !== this.sessionID) return;
    const event = push.event;
    switch (event.type) {
      case "frame.snapshot":
      case "frame.shutdown":
        this.applyFrame(event.payload);
        break;
      case "session.status":
        this.running = (event.payload as any)?.status === "running";
        if (!this.running) {
          this.inputEnabled = true;
          this.inputPlaceholder = "请输入消息并回车。";
          this.resetEscapeInterrupt();
        }
        this.renderStatus();
        this.renderTasks();
        this.renderInput();
        break;
      case "session.done":
        this.running = false;
        this.applySnapshot(event.payload);
        break;
      case "permission_requested":
        void this.showPermission(event.payload);
        break;
    }
    this.screen.render();
  }

  private applySnapshot(snapshot: any): void {
    this.sessionID = snapshot.session_id || this.sessionID;
    this.applyFrame(snapshot.frame);
  }

  private applyFrame(frame: any): void {
    if (!frame) return;
    this.lastFrame = frame;
    this.inputEnabled = frame.input_enabled !== false;
    this.inputPlaceholder = frame.input_placeholder || "请输入消息并回车。";
    this.transcriptEntries = (frame.transcript_entries || []).map((entry: any) => ({ kind: entry.kind, text: entry.text || "" }));
    this.taskLines = (frame.task_activity_entries || []).map((entry: any) => {
      const label = entry.worker_label || entry.task_id || "agent";
      const summary = entry.summary || entry.result_text || entry.status || "";
      return `${label}: ${summary}`;
    });
    this.renderTranscript();
    this.renderTasks();
    this.renderStatus(frame);
    this.renderInput();
  }

  private renderTranscript(): void {
    const previousScroll = getPaneScroll(this.transcript);
    const shouldFollow = this.transcriptFollow || isPaneAtBottom(this.transcript);
    const lines: string[] = [];
    for (const entry of this.transcriptEntries) {
      if (entry.kind === "user") {
        lines.push(`{${tuiTheme.user}-fg}{bold}你{/bold}{/${tuiTheme.user}-fg}`);
        lines.push(indent(escapeBlessedTags(entry.text)));
      } else {
        lines.push(`{${tuiTheme.assistant}-fg}{bold}Lumina{/bold}{/${tuiTheme.assistant}-fg}`);
        lines.push(indent(escapeBlessedTags(entry.text)));
      }
      lines.push("");
    }
    this.transcript.setContent(lines.join("\n"));
    this.suppressTranscriptScroll = true;
    if (shouldFollow) {
      this.transcript.setScrollPerc(100);
      this.transcriptFollow = true;
    } else {
      this.transcript.scrollTo(previousScroll);
    }
    this.suppressTranscriptScroll = false;
  }

  private renderTasks(): void {
    const previousScroll = getPaneScroll(this.tasks);
    const shouldFollow = this.tasksFollow || isPaneAtBottom(this.tasks);
    const prefix = this.running ? `${spinnerFrames[this.spinner % spinnerFrames.length]} 正在执行任务${".".repeat((this.spinner % 3) + 1)}` : "空闲";
    const lines = this.taskLines.length ? this.taskLines : [prefix];
    this.tasks.setContent(lines.join("\n"));
    this.suppressTasksScroll = true;
    if (shouldFollow) {
      this.tasks.setScrollPerc(100);
      this.tasksFollow = true;
    } else {
      this.tasks.scrollTo(previousScroll);
    }
    this.suppressTasksScroll = false;
  }

  private renderStatus(frame?: any): void {
    frame = frame || this.lastFrame;
    const model = frame?.model_name || frame?.model || "unknown";
    const used = Number(frame?.context_used_tokens || 0);
    const limit = Number(frame?.context_limit_tokens || 0);
    const ratio = limit > 0 ? Math.min(1, used / limit) : 0;
    const barWidth = 24;
    const filled = Math.round(ratio * barWidth);
    const bar = `${"=".repeat(filled)}${"-".repeat(barWidth - filled)}`;
    const state = this.running ? `${spinnerFrames[this.spinner % spinnerFrames.length]} Agent is thinking` : "输入就绪";
    this.header.setContent(`{${tuiTheme.brand}-fg}{bold}LuminaCode{/bold}{/${tuiTheme.brand}-fg}{|}{${tuiTheme.muted}-fg}${state}{/${tuiTheme.muted}-fg}`);
    this.status.setContent(`Model: ${model} | Context [${bar}] ${Math.round(ratio * 100)}% ${formatTokens(used)}/${formatTokens(limit)}`);
  }

  private layout(): void {
    const width = Math.max(40, Number((this.screen as any).width || process.stdout.columns || 100));
    const height = Math.max(18, Number((this.screen as any).height || process.stdout.rows || 30));
    const inputHeight = Math.min(5, Math.max(4, height - 13));
    const statusHeight = 3;
    const taskHeight = Math.max(4, Math.min(8, Math.floor(height * 0.18)));
    const transcriptHeight = Math.max(6, height - 1 - inputHeight - statusHeight - taskHeight);
    const left = 1;
    const boxWidth = Math.max(30, width - 2);
    setBox(this.transcript, { top: 1, left, width: boxWidth, height: transcriptHeight });
    setBox(this.tasks, { top: 1 + transcriptHeight, left, width: boxWidth, height: taskHeight });
    setBox(this.status, { top: 1 + transcriptHeight + taskHeight, left, width: boxWidth, height: statusHeight });
    setBox(this.input, { top: height - inputHeight, left, width: boxWidth, height: inputHeight });
    setBox(this.menu, { top: Math.max(2, height - inputHeight - 11), left: 6, width: Math.min(90, width - 12), height: Math.min(10, height - 8) });
  }

  private renderInput(): void {
    const runes = Array.from(this.inputBuffer);
    this.inputCursor = Math.max(0, Math.min(this.inputCursor, runes.length));
    const before = runes.slice(0, this.inputCursor).join("");
    const after = runes.slice(this.inputCursor).join("");
    const prompt = this.inputEnabled && !this.running ? "❯" : "·";
    if (this.inputBuffer === "") {
      this.input.setContent(`${prompt} ▌ ${this.inputPlaceholder}`);
      return;
    }
    this.input.setContent(`${prompt} ${before}▌${after}`);
  }

  private insertInputText(text: string): void {
    if (!text) return;
    this.resetHistoryBrowse();
    const runes = Array.from(this.inputBuffer);
    const insert = Array.from(text).filter((char) => isPrintableInput(char));
    if (insert.length === 0) return;
    runes.splice(this.inputCursor, 0, ...insert);
    this.inputBuffer = runes.join("");
    this.inputCursor += insert.length;
    this.renderInput();
    this.updateCompletion();
    this.screen.render();
  }

  private moveInputCursor(delta: number): void {
    this.inputCursor = Math.max(0, Math.min(Array.from(this.inputBuffer).length, this.inputCursor + delta));
    this.renderInput();
    this.screen.render();
  }

  private deleteBeforeCursor(): void {
    if (!this.inputEnabled || this.running || this.inputCursor <= 0) return;
    this.resetHistoryBrowse();
    const runes = Array.from(this.inputBuffer);
    runes.splice(this.inputCursor - 1, 1);
    this.inputBuffer = runes.join("");
    this.inputCursor -= 1;
    this.renderInput();
    this.updateCompletion();
    this.screen.render();
  }

  private deleteAtCursor(): void {
    if (!this.inputEnabled || this.running) return;
    this.resetHistoryBrowse();
    const runes = Array.from(this.inputBuffer);
    if (this.inputCursor >= runes.length) return;
    runes.splice(this.inputCursor, 1);
    this.inputBuffer = runes.join("");
    this.renderInput();
    this.updateCompletion();
    this.screen.render();
  }

  private async showPermission(payload: any): Promise<void> {
    this.modal.setContent(`{${tuiTheme.warning}-fg}需要权限确认{/${tuiTheme.warning}-fg}\n\n${escapeBlessedTags(formatPermissionPrompt(payload))}\n\n[o] 允许一次   [a] 总是允许   [d/esc] 拒绝`);
    this.modal.show();
    this.modal.focus();
    this.screen.render();
    const resolve = async (decision: string) => {
      this.modal.hide();
      this.input.focus();
      await this.rpc.call("permission.resolve", { session_id: this.sessionID, request_id: payload.request_id, decision });
      this.screen.render();
    };
    this.modal.onceKey("o", () => void resolve("once"));
    this.modal.onceKey("a", () => void resolve("always"));
    this.modal.onceKey("d", () => void resolve("deny"));
    this.modal.onceKey("escape", () => void resolve("deny"));
  }

  private setInput(value: string): void {
    this.inputBuffer = value;
    this.inputCursor = Array.from(value).length;
    this.renderInput();
    this.input.focus();
    this.screen.render();
  }

  private hideMenu(): void {
    this.menu.hide();
    this.menuMode = null;
    this.input.focus();
    this.screen.render();
  }

  private historyUp(): void {
    if (this.history.length === 0) return;
    if (this.historyIndex < 0) {
      this.historyDraft = this.inputBuffer;
    }
    this.historyIndex = Math.min(this.historyIndex + 1, this.history.length - 1);
    this.setInput(this.history[this.historyIndex]);
  }

  private historyDown(): void {
    if (this.historyIndex < 0) return;
    if (this.historyIndex === 0) {
      this.historyIndex = -1;
      this.setInput(this.historyDraft);
      return;
    }
    this.historyIndex -= 1;
    this.setInput(this.history[this.historyIndex]);
  }

  private resetHistoryBrowse(): void {
    if (this.historyIndex >= 0) {
      this.historyIndex = -1;
      this.historyDraft = "";
    }
  }
}

function isPrintableInput(value: string | undefined): value is string {
  if (!value) return false;
  for (const char of Array.from(value)) {
    if (/[\u0000-\u001f\u007f]/u.test(char)) return false;
  }
  return true;
}

function getPaneScroll(pane: any): number {
  const scroll = Number(pane.getScroll?.());
  if (Number.isFinite(scroll)) return Math.max(0, Math.floor(scroll));
  const childBase = Number(pane.childBase || 0);
  return Number.isFinite(childBase) ? Math.max(0, Math.floor(childBase)) : 0;
}

function isPaneAtBottom(pane: any): boolean {
  const percent = Number(pane.getScrollPerc?.());
  if (Number.isFinite(percent)) return percent >= 99;
  const scroll = getPaneScroll(pane);
  const contentHeight = Number(pane.getScrollHeight?.() || 0);
  const visibleHeight = Math.max(1, Number(pane.height || 0) - 2);
  if (!Number.isFinite(contentHeight) || contentHeight <= visibleHeight) return true;
  return scroll + visibleHeight >= contentHeight - 1;
}

function formatPermissionPrompt(payload: any): string {
  const prompt = payload?.prompt ?? payload?.skill_shell_request ?? payload;
  if (typeof prompt === "string") return prompt;
  if (prompt && typeof prompt === "object") {
    const request = (prompt as any).skill_shell_request || prompt;
    const parts: string[] = [];
    const title = (request.skill || request.skill_name || request.name) ? `Skill: ${request.skill || request.skill_name || request.name}` : "";
    if (title) parts.push(title);
    if (request.command) parts.push(`Command: ${request.command}`);
    if (request.cwd || request.workdir) parts.push(`CWD: ${request.cwd || request.workdir}`);
    if (request.reason || request.description) parts.push(String(request.reason || request.description));
    if (parts.length > 0) return parts.join("\n");
    try {
      return JSON.stringify(prompt, null, 2);
    } catch {
      return "tool permission";
    }
  }
  return "tool permission";
}

function escapeBlessedTags(text: string): string {
  return text.replace(/[{}]/g, (char) => (char === "{" ? "{open}" : "{close}"));
}

function normalizeMenuItems(items: unknown): Array<{ name: string; description: string }> {
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

function parseTerminalControlSequence(rest: string): { length: number } | null {
  if (rest.startsWith("\x1b[M")) {
    const chars = Array.from(rest);
    if (chars.length >= 6) return { length: chars.slice(0, 6).join("").length };
    return { length: rest.length };
  }
  if (rest.startsWith("[M")) {
    const chars = Array.from(rest);
    if (chars.length >= 5) return { length: chars.slice(0, 5).join("").length };
    return { length: rest.length };
  }
  const sgrMouse = /^\x1b\[<\d+(?:;\d+){0,2}[mM]/.exec(rest);
  if (sgrMouse) return { length: sgrMouse[0].length };
  const straySGRMouse = /^\[<\d+(?:;\d+){0,2}[mM]/.exec(rest);
  if (straySGRMouse) return { length: straySGRMouse[0].length };
  const urxvtMouse = /^\x1b\[\d+(?:;\d+){2}M/.exec(rest);
  if (urxvtMouse) return { length: urxvtMouse[0].length };
  const strayUrxvtMouse = /^\[\d+(?:;\d+){2}M/.exec(rest);
  if (strayUrxvtMouse) return { length: strayUrxvtMouse[0].length };
  if (rest.startsWith("\x1b[I") || rest.startsWith("\x1b[O")) {
    return { length: 3 };
  }
  if (rest.startsWith("[I") || rest.startsWith("[O")) {
    return { length: 2 };
  }
  return null;
}

function parseCursorSequence(rest: string): { name: "up" | "down" | "right" | "left" | "home" | "end"; length: number } | null {
  const applicationCursor: Record<string, "up" | "down" | "right" | "left" | "home" | "end"> = {
    "\x1bOA": "up",
    "\x1bOB": "down",
    "\x1bOC": "right",
    "\x1bOD": "left",
    "\x1bOH": "home",
    "\x1bOF": "end",
  };
  for (const [sequence, name] of Object.entries(applicationCursor)) {
    if (rest.startsWith(sequence)) return { name, length: sequence.length };
  }
  const csi = /^\x1b\[[0-9;?]*([ABCDHF])/.exec(rest);
  if (!csi) return null;
  const key = csi[1];
  const names: Record<string, "up" | "down" | "right" | "left" | "home" | "end"> = {
    A: "up",
    B: "down",
    C: "right",
    D: "left",
    H: "home",
    F: "end",
  };
  return { name: names[key], length: csi[0].length };
}

function createTheme(): {
  brand: string;
  user: string;
  assistant: string;
  background: string;
  panelBg: string;
  text: string;
  muted: string;
  panelBorder: string;
  panelLabel: string;
  subtleBorder: string;
  inputBorder: string;
  warning: string;
  selectionBg: string;
  selectionFg: string;
} {
  const forced = (process.env.LUMINA_TUI_THEME || "").toLowerCase();
  const dark = forced === "dark" || (forced !== "light" && isMacOSDarkMode());
  if (dark) {
    return {
      brand: "cyan",
      user: "cyan",
      assistant: "green",
      background: "black",
      panelBg: "black",
      text: "white",
      muted: "gray",
      panelBorder: "gray",
      panelLabel: "cyan",
      subtleBorder: "gray",
      inputBorder: "green",
      warning: "yellow",
      selectionBg: "cyan",
      selectionFg: "black",
    };
  }
  return {
    brand: "blue",
    user: "blue",
    assistant: "green",
    background: "white",
    panelBg: "white",
    text: "black",
    muted: "gray",
    panelBorder: "gray",
    panelLabel: "blue",
    subtleBorder: "gray",
    inputBorder: "green",
    warning: "yellow",
    selectionBg: "blue",
    selectionFg: "white",
  };
}

function isMacOSDarkMode(): boolean {
  if (process.platform !== "darwin") {
    return true;
  }
  try {
    return execFileSync("defaults", ["read", "-g", "AppleInterfaceStyle"], { encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] })
      .trim()
      .toLowerCase() === "dark";
  } catch {
    return false;
  }
}
