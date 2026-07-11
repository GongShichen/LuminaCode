import { escInterruptWindowMs, spinnerFrames } from "./constants";
import { escapeBlessedTags, formatPermissionPrompt, normalizeMenuItems } from "./formatters";
import { isPrintableInput, setBracketedPaste } from "./input";
import { RawInputDispatcher } from "./raw-input";
import { RpcClient } from "./rpc";
import { buildHeaderContent, buildStatusContent, buildTasksContent, buildTranscriptContent, formatGateSummary } from "./rendering";
import { getPaneScroll, isPaneAtBottom } from "./scroll";
import { createTheme } from "./theme";
import type { LaunchOptions, PushEvent, TranscriptEntry } from "./types";
import { setBox } from "./utils";
import { createTuiWidgets } from "./widgets";
import type { TuiWidgets } from "./widgets";

const tuiTheme = createTheme();
const renderFrameMs = 33;
const paneScrollHoldMs = 1500;

export class LuminaTui {
  private screen: TuiWidgets["screen"];
  private header: TuiWidgets["header"];
  private transcript: TuiWidgets["transcript"];
  private tasks: TuiWidgets["tasks"];
  private status: TuiWidgets["status"];
  private input: TuiWidgets["input"];
  private menu: TuiWidgets["menu"];
  private modal: TuiWidgets["modal"];

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
  private menuMode: "slash" | "skill" | "resume" | "team" | "memory" | null = null;
  private inputBuffer = "";
  private inputCursor = 0;
  private inputScroll = 0;
  private inputEnabled = true;
  private inputPlaceholder = "请输入消息并回车。";
  private inputDataHandler?: (chunk: Buffer | string) => void;
  private rawInput: RawInputDispatcher;
  private transcriptFollow = true;
  private tasksFollow = true;
  private suppressTranscriptScroll = false;
  private suppressTasksScroll = false;
  private transcriptScrollHoldUntil = 0;
  private tasksScrollHoldUntil = 0;
  private transcriptRenderPending = false;
  private tasksRenderPending = false;
  private statusRenderPending = false;
  private deferredRenderPending = false;
  private paneHoldTimer?: NodeJS.Timeout;
  private renderTimer?: NodeJS.Timeout;
  private lastRenderAt = 0;
  private localSubmitPending = false;
  private transcriptContent = "";
  private tasksContent = "";
  private statusContent = "";
  private headerContent = "";
  private lastEscapeAt = 0;
  private escapeResetTimer?: NodeJS.Timeout;
  private modalKeyHandlers = new Map<string, () => void>();
  private teamMode = false;
  private teamSessionID = "";
  private activeTeamName = "";
  private teamLoopIteration = 0;
  private teamDialogueEntries: any[] = [];
  private teamActivityRows: any[] = [];
  private teamArtifacts: any[] = [];
  private teamGateStatus: any = {};
  private teamContract: any = null;
  private teamGateVerdicts: any = {};
  private teamStreamingText = new Map<string, string>();
  private pendingPrompt: "new-team-name" | null = null;
  private memoryItems: any[] = [];

  constructor(private rpc: RpcClient, private options: LaunchOptions) {
    const widgets = createTuiWidgets(tuiTheme);
    this.screen = widgets.screen;
    this.header = widgets.header;
    this.transcript = widgets.transcript;
    this.tasks = widgets.tasks;
    this.status = widgets.status;
    this.input = widgets.input;
    this.menu = widgets.menu;
    this.modal = widgets.modal;
    this.rawInput = new RawInputDispatcher({
      insertPastedText: (text) => this.insertPastedText(text),
      handleWheel: (direction, data) => this.handleWheel(direction, data),
      handleKey: (ch, key) => this.handleInputKey(ch, key),
      exit: () => this.exit(),
    });
  }

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
    this.rpc.onDisconnect((reason) => this.handleBackendDisconnect(reason));
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
      this.requestRender();
    }, 140);
    this.requestRender(true);
  }

  private bindKeys(): void {
    this.screen.on("resize", () => {
      this.layout();
      this.requestRender(true);
    });
    this.screen.on("mouse", () => {
      // Registered intentionally so blessed enables terminal mouse reporting.
      // Wheel routing is handled from the raw input stream to avoid duplicate pane renders.
    });
    this.transcript.on("scroll", () => {
      if (!this.suppressTranscriptScroll) {
        this.transcriptFollow = isPaneAtBottom(this.transcript);
        if (!this.transcriptFollow) this.holdPaneAutoRefresh("transcript");
      }
    });
    this.tasks.on("scroll", () => {
      if (!this.suppressTasksScroll) {
        this.tasksFollow = isPaneAtBottom(this.tasks);
        if (!this.tasksFollow) this.holdPaneAutoRefresh("tasks");
      }
    });
  }

  private requestRender(immediate = false, allowDuringQuiet = false): void {
    if (immediate) {
      if (this.renderTimer) {
        clearTimeout(this.renderTimer);
        this.renderTimer = undefined;
      }
      this.lastRenderAt = Date.now();
      this.screen.render();
      return;
    }
    if (this.quietScrollActive() && !allowDuringQuiet) {
      this.deferredRenderPending = true;
      this.schedulePaneHoldFlush();
      return;
    }
    if (this.renderTimer) return;
    const wait = Math.max(0, renderFrameMs - (Date.now() - this.lastRenderAt));
    this.renderTimer = setTimeout(() => {
      this.renderTimer = undefined;
      this.lastRenderAt = Date.now();
      this.screen.render();
    }, wait);
  }

  private holdPaneAutoRefresh(pane: "transcript" | "tasks"): void {
    const until = Date.now() + paneScrollHoldMs;
    if (pane === "transcript") {
      this.transcriptScrollHoldUntil = until;
    } else {
      this.tasksScrollHoldUntil = until;
    }
    this.schedulePaneHoldFlush();
    this.requestRender(false, true);
  }

  private schedulePaneHoldFlush(): void {
    const now = Date.now();
    const nextUntil = Math.max(this.transcriptScrollHoldUntil, this.tasksScrollHoldUntil);
    if (nextUntil <= now) return;
    if (this.paneHoldTimer) clearTimeout(this.paneHoldTimer);
    this.paneHoldTimer = setTimeout(() => {
      this.paneHoldTimer = undefined;
      this.flushHeldPaneRenders();
    }, Math.max(1, nextUntil - now + 5));
  }

  private flushHeldPaneRenders(): void {
    const now = Date.now();
    if (this.transcriptRenderPending && now >= this.transcriptScrollHoldUntil) {
      this.transcriptRenderPending = false;
      this.renderTranscript(true);
    }
    if (this.tasksRenderPending && now >= this.tasksScrollHoldUntil) {
      this.tasksRenderPending = false;
      this.renderTasks(true);
    }
    if (this.statusRenderPending && !this.quietScrollActive()) {
      this.statusRenderPending = false;
      this.renderStatus(undefined, true);
    }
    if (this.transcriptRenderPending || this.tasksRenderPending || this.statusRenderPending) {
      this.schedulePaneHoldFlush();
    }
    if (!this.quietScrollActive() && this.deferredRenderPending) {
      this.deferredRenderPending = false;
      this.requestRender(true);
      return;
    }
    this.requestRender();
  }

  private quietScrollActive(): boolean {
    const now = Date.now();
    return now < this.transcriptScrollHoldUntil || now < this.tasksScrollHoldUntil;
  }

  private handleWheel(direction: "up" | "down", data: any): void {
    const delta = direction === "up" ? -1 : 1;
    if (!this.modal.hidden && this.pointInside(this.modal, data)) {
      this.modal.scroll(delta * 2);
      this.requestRender(false, true);
      return;
    }
    if (this.pointInside(this.input, data)) {
      if (this.inputCanScroll()) {
        this.inputScroll = Math.max(0, this.inputScroll + delta);
        this.input.scrollTo(this.inputScroll);
        this.requestRender(false, true);
      }
      return;
    }
    if (this.pointInside(this.tasks, data)) {
      this.tasks.scroll(delta * 2);
      this.tasksFollow = isPaneAtBottom(this.tasks);
      if (!this.tasksFollow) this.holdPaneAutoRefresh("tasks");
      this.requestRender(false, true);
      return;
    }
    if (this.pointInside(this.transcript, data)) {
      this.scrollTranscriptBy(delta * 3);
    }
  }

  private scrollTranscriptBy(delta: number): void {
    this.transcript.scroll(delta);
    this.transcriptFollow = isPaneAtBottom(this.transcript);
    if (!this.transcriptFollow) this.holdPaneAutoRefresh("transcript");
    this.requestRender(false, true);
  }

  private pointInside(pane: any, data: any): boolean {
    const pos = pane?.lpos;
    if (!pos || data == null) return false;
    const x = Number(data.x);
    const y = Number(data.y);
    if (!Number.isFinite(x) || !Number.isFinite(y)) return false;
    const xi = Number(pos.xi);
    const xl = Number(pos.xl);
    const yi = Number(pos.yi);
    const yl = Number(pos.yl);
    if (![xi, xl, yi, yl].every(Number.isFinite)) return false;
    return x >= xi && x <= xl && y >= yi && y <= yl;
  }

  private inputCanScroll(): boolean {
    if (!this.inputBuffer.includes("\n")) return false;
    const visibleHeight = Math.max(1, Number(this.input.height || 0) - 2);
    return this.inputBuffer.split("\n").length > visibleHeight;
  }

  private prepareTerminalInput(): void {
    const input = (this.screen.program as any)?.input || process.stdin;
    input.setEncoding?.("utf8");
    input.resume?.();
    if (input.isTTY && typeof input.setRawMode === "function") {
      input.setRawMode(true);
    }
    setBracketedPaste(input, true);
    this.setTerminalMouse(true);
    this.inputDataHandler = (chunk: Buffer | string) => {
      void this.rawInput.handle(String(chunk));
    };
    input.on?.("data", this.inputDataHandler);
    process.once("exit", () => {
      if (this.inputDataHandler) {
        input.off?.("data", this.inputDataHandler);
      }
      if (input.isTTY && typeof input.setRawMode === "function") {
        input.setRawMode(false);
      }
      this.setTerminalMouse(false);
      setBracketedPaste(input, false);
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
    this.setTerminalMouse(false);
    setBracketedPaste(input, false);
    process.exit(0);
  }

  private setTerminalMouse(enabled: boolean): void {
    const program = (this.screen as any).program;
    try {
      if (enabled) {
        program?.disableMouse?.();
        program?.setMouse?.({ vt200Mouse: true, sgrMouse: true, cellMotion: false, allMotion: false, utfMouse: false }, true);
      } else {
        program?.setMouse?.({ vt200Mouse: false, sgrMouse: false, cellMotion: false, allMotion: false, utfMouse: false }, false);
        program?.disableMouse?.();
      }
    } catch {
      // Mouse reporting is terminal-dependent; failure should not break keyboard input.
    }
  }

  private async exitFromCommand(): Promise<void> {
    try {
      await this.rpc.call("session.exit", { session_id: this.sessionID });
    } catch {
      // Exit should still work if the backend is already gone.
    }
    this.exit();
  }

  private async handleInputKey(ch: string | undefined, key: any): Promise<void> {
    const name = key?.full || key?.name || "";
    if (!this.modal.hidden) {
      const handler = this.modalKeyHandlers.get(name) || this.modalKeyHandlers.get(name.toLowerCase());
      if (handler) {
        handler();
      } else if (name === "up") {
        this.modal.scroll(-1);
        this.requestRender(true);
      } else if (name === "down") {
        this.modal.scroll(1);
        this.requestRender(true);
      } else if (name === "escape") {
        this.closeModal();
      }
      return;
    }
    if (!this.menu.hidden && this.menuMode) {
      switch (name) {
        case "up":
          this.menu.up(1);
          this.requestRender(true);
          return;
        case "down":
          this.menu.down(1);
          this.requestRender(true);
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
        if (this.running) {
          this.scrollTranscriptBy(-3);
          return;
        }
        this.historyUp();
        return;
      case "down":
        if (this.running) {
          this.scrollTranscriptBy(3);
          return;
        }
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
        this.requestRender(true);
        return;
      case "end":
      case "C-e":
        this.inputCursor = Array.from(this.inputBuffer).length;
        this.renderInput();
        this.requestRender(true);
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
    if (this.pendingPrompt === "new-team-name") {
      await this.createNewTeamTemplate(text);
      return;
    }
    if (text === "/quit" || text === "/exit" || text === "/q") {
      if (this.teamMode && this.running) {
        try {
          await this.rpc.call("team.abort", { team_session_id: this.teamSessionID });
        } catch {
          // Session exit continues even if team abort already completed.
        }
      }
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
    const lower = text.toLowerCase();
    if (lower === "/storage") {
      const report = await this.rpc.call("storage.status");
      this.pushStorageReport(report, false);
      this.setInput("");
      return;
    }
    if (lower === "/cleanup" || lower === "/cleanup --enforce") {
      const report = await this.rpc.call("storage.cleanup", { enforce: lower.includes("--enforce") });
      this.pushStorageReport(report, lower.includes("--enforce"));
      this.setInput("");
      return;
    }
    if (lower === "/memory") {
      await this.showMemoryList();
      this.setInput("");
      return;
    }
    if (lower.startsWith("/memorysearch")) {
      await this.searchMemory(text.replace(/^\/memorysearch/i, "").trim());
      this.setInput("");
      return;
    }
    if (lower.startsWith("/memoryforget")) {
      await this.forgetMemory(text.replace(/^\/memoryforget/i, "").trim());
      this.setInput("");
      return;
    }
    if (lower.startsWith("/memoryapprove")) {
      await this.simpleMemoryAction("memory.approve", "memory.approve", text.replace(/^\/memoryapprove/i, "").trim());
      this.setInput("");
      return;
    }
    if (lower.startsWith("/memoryrestore")) {
      await this.simpleMemoryAction("memory.restore", "memory.restore", text.replace(/^\/memoryrestore/i, "").trim());
      this.setInput("");
      return;
    }
    if (lower.startsWith("/memoryprioritize")) {
      await this.prioritizeMemory(text.replace(/^\/memoryprioritize/i, "").trim());
      this.setInput("");
      return;
    }
    if (lower.startsWith("/memorydeprioritize")) {
      await this.simpleMemoryAction("memory.deprioritize", "memory.deprioritize", text.replace(/^\/memorydeprioritize/i, "").trim());
      this.setInput("");
      return;
    }
    if (lower.startsWith("/memorysupersede")) {
      await this.supersedeMemory(text.replace(/^\/memorysupersede/i, "").trim());
      this.setInput("");
      return;
    }
    if (lower.startsWith("/memoryexport")) {
      await this.exportMemory(text.replace(/^\/memoryexport/i, "").trim());
      this.setInput("");
      return;
    }
    if (lower.startsWith("/memoryimport")) {
      await this.importMemory(text.replace(/^\/memoryimport/i, "").trim());
      this.setInput("");
      return;
    }
    if (lower === "/pin" || lower === "/unpin") {
      const meta = await this.rpc.call("session.pin", { session_id: this.sessionID, pinned: lower === "/pin" });
      this.taskLines.push(`session ${meta?.session_id || this.sessionID}: ${meta?.pinned ? "pinned" : "unpinned"}`);
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
    if (lower === "/skill") {
      await this.showSkills();
      return;
    }
    if (lower === "/team") {
      await this.showTeams();
      return;
    }
    if (lower === "/newteam") {
      this.startNewTeamPrompt();
      return;
    }
    if (lower === "/teamsummary") {
      if (!this.teamMode || !this.teamSessionID) {
        this.taskLines.push("/TeamSummary: 仅在 Team 模式下可用。请先使用 /team 进入 Team 模式。");
        this.renderTasks();
        this.setInput("");
        return;
      }
      try {
        const summary = await this.rpc.call("team.summary", { team_session_id: this.teamSessionID });
        const lines = [
          `Team: ${summary.active_team_name || "unknown"}`,
          `Loop Iteration: ${summary.loop_iteration ?? 0}`,
          `Running: ${summary.running ? "yes" : "no"}`,
          `Dialogue Count: ${summary.dialogue_count ?? 0}`,
          `Artifact Count: ${summary.artifact_count ?? 0}`,
          `Activity Count: ${summary.activity_count ?? 0}`,
          `Gates: ${formatGateSummary(summary.gate_verdicts, summary.gate_status)}`,
        ];
        this.taskLines.push(...lines.map((line) => `team.summary: ${line}`));
        this.renderTasks();
      } catch (err) {
        this.taskLines.push(`team.summary: ${String(err)}`);
        this.renderTasks();
      }
      this.setInput("");
      return;
    }
    if (lower === "/teamout") {
      await this.handleTeamOut();
      return;
    }
    if (lower === "/resume") {
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
    this.localSubmitPending = true;
    this.transcriptFollow = true;
    this.tasksFollow = true;
    this.inputEnabled = false;
    this.inputPlaceholder = this.teamMode ? "Team is working..." : "Agent is responding...";
    this.renderStatus();
    this.renderInput();
    try {
      if (this.teamMode) {
        await this.rpc.call("team.submit", { team_session_id: this.teamSessionID, input: text });
      } else {
        await this.rpc.call("session.submit", { session_id: this.sessionID, input: text });
      }
    } catch (err) {
      this.taskLines.push(String(err));
      this.localSubmitPending = false;
      this.running = false;
      this.inputEnabled = true;
      this.renderTasks();
      this.renderStatus();
      this.renderInput();
    }
  }

  private async handleEscape(): Promise<void> {
    if (!this.running) {
      if (this.pendingPrompt) {
        this.pendingPrompt = null;
        this.inputPlaceholder = this.teamMode ? "请输入 Team 消息并回车。" : "请输入消息并回车。";
        this.setInput("");
      }
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
      this.requestRender(true);
      try {
        if (this.teamMode && this.teamSessionID) {
          await this.rpc.call("team.abort", { team_session_id: this.teamSessionID });
        } else {
          await this.rpc.call("session.abort", { session_id: this.sessionID });
        }
        this.localSubmitPending = false;
        this.running = false;
        this.inputEnabled = true;
        this.inputPlaceholder = this.teamMode ? "请输入 Team 消息并回车。" : "请输入消息并回车。";
        this.renderInput();
        this.renderStatus();
      } catch (err) {
        this.taskLines.push(`abort: ${String(err)}`);
        this.renderTasks();
      }
      return;
    }
    this.lastEscapeAt = now;
    this.inputPlaceholder = "再按一次 Esc 中断当前会话。";
    this.renderInput();
    this.requestRender(true);
    if (this.escapeResetTimer) clearTimeout(this.escapeResetTimer);
    this.escapeResetTimer = setTimeout(() => {
      this.lastEscapeAt = 0;
      if (this.running) {
        this.inputPlaceholder = "Agent is responding...";
        this.renderInput();
        this.requestRender(true);
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
    if (this.pendingPrompt) {
      this.hideMenu();
      return;
    }
    const value = this.inputBuffer;
    if (!value.startsWith("/") || value.includes(" ")) {
      this.hideMenu();
      return;
    }
    const lower = value.toLowerCase();
    const matches = this.slashItems.filter((item) => item.name.toLowerCase().startsWith(lower)).slice(0, 10);
    if (matches.length === 0) {
      this.hideMenu();
      return;
    }
    this.menuMode = "slash";
    this.menu.setItems(matches.map((item) => `${item.name.padEnd(18)} ${item.description || ""}`));
    this.menu.select(0);
    this.menu.show();
    this.requestRender(true);
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
    if (this.menuMode === "team") {
      const teamName = this.menu.getItem(index)?.getText().trim().split(/\s+/)[0];
      if (teamName) await this.enterTeam(teamName);
      this.hideMenu();
      this.setInput("");
      return;
    }
    if (this.menuMode === "memory") {
      const memoryID = this.menu.getItem(index)?.getText().trim().split(/\s+/)[0];
      if (memoryID) await this.showMemoryDetail(memoryID);
      return;
    }
    const selected = this.menu.getItem(index)?.getText().trim().split(/\s+/)[0];
    if (!selected) return;
    const lower = selected.toLowerCase();
    if (lower === "/skill") {
      await this.showSkills();
      return;
    }
    if (lower === "/team") {
      await this.showTeams();
      return;
    }
    if (lower === "/teamsummary") {
      this.setInput("/TeamSummary");
      this.hideMenu();
      return;
    }
    if (lower === "/newteam") {
      this.hideMenu();
      this.startNewTeamPrompt();
      return;
    }
    if (lower === "/teamout") {
      this.setInput("/TeamOut");
      this.hideMenu();
      return;
    }
    if (lower === "/resume") {
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
    this.requestRender(true);
  }

  private pushStorageReport(report: any, enforced: boolean): void {
    const total = formatBytes(Number(report?.total_bytes || 0));
    const actions = Array.isArray(report?.actions) ? report.actions : [];
    this.taskLines.push(`storage: ${report?.session_count || 0} sessions, ${total}`);
    if (enforced) {
      this.taskLines.push(`cleanup: deleted ${report?.deleted_count || 0} sessions, freed ${formatBytes(Number(report?.freed_bytes || 0))}`);
    } else {
      this.taskLines.push(`cleanup: dry-run, ${actions.length} planned actions`);
    }
    for (const action of actions.slice(0, 12)) {
      const id = action?.session_id || String(action?.path || "").split("/").pop() || "unknown";
      const verb = enforced ? (action?.deleted ? "removed" : action?.error ? "error" : "skipped") : "would remove";
      this.taskLines.push(`cleanup: ${verb} ${id} ${formatBytes(Number(action?.bytes || 0))} - ${action?.reason || ""}`);
    }
    if (actions.length > 12) this.taskLines.push(`cleanup: ... ${actions.length - 12} more`);
    this.renderTasks(true);
  }

  private async showMemoryList(): Promise<void> {
    try {
      const result = await this.rpc.call("memory.list", { session_id: this.sessionID, include_inactive: true, limit: 12 });
      this.showMemoryMenu("memory", result?.items);
    } catch (err) {
      this.taskLines.push(`memory: ${String(err)}`);
      this.renderTasks(true);
    }
  }

  private async searchMemory(query: string): Promise<void> {
    if (!query) {
      this.taskLines.push("memory.search: query required");
      this.renderTasks(true);
      return;
    }
    try {
      const result = await this.rpc.call("memory.search", { session_id: this.sessionID, query, limit: 12 });
      this.showMemoryMenu(`memory.search: ${query}`, result?.items);
      const run = result?.retrieval_trace?.run;
      if (run) {
        const channels = Array.isArray(run.channel_results) ? run.channel_results.length : 0;
        const selectedAtoms = Array.isArray(run.coverage_ledger?.selected) ? run.coverage_ledger.selected.length : 0;
        const uncovered = Array.isArray(run.coverage_ledger?.uncovered) ? run.coverage_ledger.uncovered.length : 0;
        this.taskLines.push(`memory.trace: channels=${channels} atoms=${selectedAtoms} uncovered=${uncovered} vector_batches=${run.embedding_trace?.batches || 0}`);
      }
    } catch (err) {
      this.taskLines.push(`memory.search: ${String(err)}`);
      this.renderTasks(true);
    }
  }

  private async forgetMemory(args: string): Promise<void> {
    const parts = args.split(/\s+/).filter(Boolean);
    const memoryID = parts.find((part) => part !== "--hard") || "";
    const hard = parts.includes("--hard");
    if (!memoryID) {
      this.taskLines.push("memory.forget: memory_id required");
      this.renderTasks(true);
      return;
    }
    try {
      await this.rpc.call("memory.delete", { memory_id: memoryID, hard });
      this.taskLines.push(`memory.forget: ${hard ? "hard deleted" : "deleted"} ${memoryID}`);
      this.renderTasks(true);
    } catch (err) {
      this.taskLines.push(`memory.forget: ${String(err)}`);
      this.renderTasks(true);
    }
  }

  private async simpleMemoryAction(method: string, label: string, args: string): Promise<void> {
    const memoryID = args.split(/\s+/).filter(Boolean)[0] || "";
    if (!memoryID) {
      this.taskLines.push(`${label}: memory_id required`);
      this.renderTasks(true);
      return;
    }
    try {
      await this.rpc.call(method, { memory_id: memoryID });
      this.taskLines.push(`${label}: ${memoryID}`);
      this.renderTasks(true);
    } catch (err) {
      this.taskLines.push(`${label}: ${String(err)}`);
      this.renderTasks(true);
    }
  }

  private async prioritizeMemory(args: string): Promise<void> {
    const parts = args.split(/\s+/).filter(Boolean);
    const memoryID = parts[0] || "";
    const importance = Number(parts[1] || "1");
    if (!memoryID) {
      this.taskLines.push("memory.prioritize: memory_id required");
      this.renderTasks(true);
      return;
    }
    try {
      await this.rpc.call("memory.prioritize", { memory_id: memoryID, importance: Number.isFinite(importance) ? importance : 1 });
      this.taskLines.push(`memory.prioritize: ${memoryID}`);
      this.renderTasks(true);
    } catch (err) {
      this.taskLines.push(`memory.prioritize: ${String(err)}`);
      this.renderTasks(true);
    }
  }

  private async supersedeMemory(args: string): Promise<void> {
    const parts = args.split(/\s+/).filter(Boolean);
    if (parts.length < 2) {
      this.taskLines.push("memory.supersede: old_memory_id new_memory_id required");
      this.renderTasks(true);
      return;
    }
    try {
      await this.rpc.call("memory.supersede", { old_memory_id: parts[0], new_memory_id: parts[1] });
      this.taskLines.push(`memory.supersede: ${parts[0]} -> ${parts[1]}`);
      this.renderTasks(true);
    } catch (err) {
      this.taskLines.push(`memory.supersede: ${String(err)}`);
      this.renderTasks(true);
    }
  }

  private async exportMemory(outDir: string): Promise<void> {
    try {
      const result = await this.rpc.call("memory.export", { format: "markdown", out_dir: outDir });
      this.taskLines.push(`memory.export: ${result?.path || "done"}`);
      this.renderTasks(true);
    } catch (err) {
      this.taskLines.push(`memory.export: ${String(err)}`);
      this.renderTasks(true);
    }
  }

  private async importMemory(path: string): Promise<void> {
    if (!path) {
      this.taskLines.push("memory.import: path required");
      this.renderTasks(true);
      return;
    }
    try {
      const result = await this.rpc.call("memory.import", { path });
      this.taskLines.push(`memory.import: imported ${result?.imported || 0}`);
      this.renderTasks(true);
    } catch (err) {
      this.taskLines.push(`memory.import: ${String(err)}`);
      this.renderTasks(true);
    }
  }

  private pushMemoryItems(prefix: string, items: any): void {
    const list = Array.isArray(items) ? items : [];
    this.taskLines.push(`${prefix}: ${list.length} item(s)`);
    for (const item of list.slice(0, 12)) {
      const id = item?.memory_id || "unknown";
      const scope = `${item?.scope_type || "?"}/${item?.scope_key || "?"}`;
      const typ = item?.memory_type || "?";
      const status = item?.status || "?";
      const title = item?.title || item?.summary || "";
      const temperature = item?.temperature || "warm";
      this.taskLines.push(`memory: ${id} [${status}/${temperature}] ${scope} ${typ} - ${title}`);
    }
    this.renderTasks(true);
  }

  private showMemoryMenu(prefix: string, items: any): void {
    const list = Array.isArray(items) ? items : [];
    this.memoryItems = list;
    if (list.length === 0) {
      this.taskLines.push(`${prefix}: no memories`);
      this.renderTasks(true);
      return;
    }
    this.taskLines.push(`${prefix}: ${list.length} item(s), Enter 查看详情`);
    this.renderTasks(true);
    this.menuMode = "memory";
    this.menu.setItems(list.map((item) => {
      const id = String(item?.memory_id || "unknown");
      const status = String(item?.status || "?");
      const temperature = String(item?.temperature || "warm");
      const scope = `${item?.scope_type || "?"}/${item?.scope_key || "?"}`;
      const typ = String(item?.memory_type || "?");
      const title = escapeBlessedTags(String(item?.title || item?.summary || ""));
      return `${id}  [${status}/${temperature}] ${scope} ${typ}  ${title}`;
    }));
    this.menu.select(0);
    this.menu.show();
    this.menu.focus();
    this.requestRender(true);
  }

  private async showMemoryDetail(memoryID: string): Promise<void> {
    try {
      const [item, lifecycle] = await Promise.all([
        this.rpc.call("memory.get", { memory_id: memoryID }),
        this.rpc.call("memory.lifecycle", { memory_id: memoryID, limit: 12 }),
      ]);
      const events = Array.isArray(lifecycle?.events) ? lifecycle.events : [];
      const lines = [
        `{${tuiTheme.brand}-fg}${escapeBlessedTags(item?.title || memoryID)}{/${tuiTheme.brand}-fg}`,
        "",
        `ID: ${item?.memory_id || memoryID}`,
        `Status: ${item?.status || "?"}`,
        `Temperature: ${item?.temperature || "warm"}  Value: ${Number(item?.value_score || 0).toFixed(3)}  Pinned: ${item?.pinned ? "yes" : "no"}`,
        `Scope: ${item?.scope_type || "?"}/${item?.scope_key || "?"}`,
        `Type: ${item?.memory_type || "?"}`,
        `Importance: ${item?.importance ?? "?"}  Confidence: ${item?.confidence ?? "?"}`,
        `Source session: ${item?.source_session_id || "-"}`,
        `Source agent: ${item?.source_agent_id || "-"}`,
        `Updated: ${item?.updated_at || "-"}`,
        `Valid until: ${item?.valid_until || "-"}`,
        `Retention expires: ${item?.retention_expires_at || "-"}`,
        `Accesses: ${item?.access_count ?? 0}  Last accessed: ${item?.last_accessed_at || "-"}`,
        `Last reinforced: ${item?.last_reinforced_at || "-"}`,
        `Archived: ${item?.archived_at || "-"}  Reason: ${item?.archive_reason || "-"}`,
        "",
        `{${tuiTheme.muted}-fg}[n] pin  [u] unpin  [a] approve  [r] restore  [x] archive  [d] delete  [h] hard delete  [p] prioritize  [l] deprioritize  [esc] close{/${tuiTheme.muted}-fg}`,
        "",
        escapeBlessedTags(item?.summary || ""),
        "",
        escapeBlessedTags(item?.content || ""),
        "",
        `{${tuiTheme.brand}-fg}Lifecycle events{/${tuiTheme.brand}-fg}`,
        ...events.map((event: any) => `${event?.created_at || "-"}  ${event?.event_type || "?"}  ${event?.old_status || "-"}/${event?.old_temperature || "-"} -> ${event?.new_status || "-"}/${event?.new_temperature || "-"}`),
      ];
      this.modal.setContent(lines.join("\n"));
      this.modal.scrollTo(0);
      this.modal.show();
      this.modal.focus();
      this.requestRender(true);
      const run = (fn: () => Promise<void>) => void fn();
      this.setModalHandlers({
        n: () => run(() => this.applyMemoryModalAction("memory.pin", memoryID)),
        u: () => run(() => this.applyMemoryModalAction("memory.unpin", memoryID)),
        a: () => run(() => this.applyMemoryModalAction("memory.approve", memoryID)),
        r: () => run(() => this.applyMemoryModalAction("memory.restore", memoryID)),
        x: () => run(() => this.applyMemoryModalAction("memory.archive", memoryID)),
        d: () => run(() => this.applyMemoryModalAction("memory.delete", memoryID)),
        h: () => run(() => this.applyMemoryModalAction("memory.delete", memoryID, { hard: true })),
        p: () => run(() => this.applyMemoryModalAction("memory.prioritize", memoryID, { importance: 1 })),
        l: () => run(() => this.applyMemoryModalAction("memory.deprioritize", memoryID)),
        escape: () => this.closeModal(),
      });
    } catch (err) {
      this.taskLines.push(`memory.detail: ${String(err)}`);
      this.renderTasks(true);
    }
  }

  private async applyMemoryModalAction(method: string, memoryID: string, extra: Record<string, any> = {}): Promise<void> {
    try {
      await this.rpc.call(method, { memory_id: memoryID, ...extra });
      this.closeModal();
      this.taskLines.push(`${method}: ${memoryID}`);
      this.renderTasks(true);
      await this.showMemoryList();
    } catch (err) {
      this.taskLines.push(`${method}: ${String(err)}`);
      this.renderTasks(true);
    }
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
    this.requestRender(true);
  }

  private async showTeams(): Promise<void> {
    const teams = await this.rpc.call("team.list");
    const items = (Array.isArray(teams) ? teams : []).flatMap((team: any) => {
      const name = typeof team?.name === "string" ? team.name : "";
      if (!name) return [];
      const display = team.display_name || name;
      const count = Number(team.agent_count || 0);
      const description = team.description || "";
      return [`${name}  ${display}  agents:${count}  ${escapeBlessedTags(description)}`];
    });
    this.menuMode = "team";
    this.menu.setItems(items.length ? items : ["No teams available"]);
    this.menu.select(0);
    this.menu.show();
    this.menu.focus();
    this.requestRender(true);
  }

  private startNewTeamPrompt(): void {
    this.pendingPrompt = "new-team-name";
    this.hideMenu();
    this.inputPlaceholder = "Team Name:";
    this.setInput("");
  }

  private async createNewTeamTemplate(name: string): Promise<void> {
    try {
      const result = await this.rpc.call("team.create_template", { name });
      const path = result?.path || "";
      const teamName = result?.team_name || name;
      const count = Number(result?.agent_count || 1);
      this.taskLines.push(`NewTeam: created ${teamName} (${count} agent)`);
      if (path) this.taskLines.push(`NewTeam: ${path}`);
      this.pendingPrompt = null;
      this.inputPlaceholder = this.teamMode ? "请输入 Team 消息并回车。" : "请输入消息并回车。";
      this.setInput("");
      this.renderTasks(true);
      await this.loadSlashItems();
    } catch (err) {
      this.taskLines.push(`NewTeam: ${String(err)}`);
      this.renderTasks(true);
      this.setInput("");
      this.inputPlaceholder = "Team Name:";
      this.pendingPrompt = "new-team-name";
    }
  }

  private async enterTeam(teamName: string): Promise<void> {
    if (teamName === "No") return;
    const snapshot = await this.rpc.call("team.start", { session_id: this.sessionID, team_name: teamName, cwd: this.options.cwd });
    this.applyTeamSnapshot(snapshot);
  }

  private async handleTeamOut(): Promise<void> {
    if (!this.teamMode || !this.teamSessionID) {
      this.setInput("");
      return;
    }
    const abort = this.running;
    if (abort) {
      const confirmed = await this.confirmModal("Team 正在执行", "退出 Team 模式会中断当前 Team Loop。\n确认中断并退出吗？");
      if (!confirmed) {
        this.setInput("");
        return;
      }
      await this.rpc.call("team.abort", { team_session_id: this.teamSessionID });
    }
    await this.rpc.call("team.out", { team_session_id: this.teamSessionID, abort });
    this.teamMode = false;
    this.teamSessionID = "";
    this.activeTeamName = "";
    this.teamDialogueEntries = [];
    this.teamActivityRows = [];
    this.teamArtifacts = [];
    this.teamGateStatus = {};
    this.teamContract = null;
    this.teamGateVerdicts = {};
    this.teamStreamingText.clear();
    this.teamLoopIteration = 0;
    this.running = false;
    this.inputEnabled = true;
    this.inputPlaceholder = "请输入消息并回车。";
    this.setInput("");
    this.renderTranscript();
    this.renderTasks();
    this.renderStatus();
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
          this.localSubmitPending = false;
          this.inputEnabled = true;
          this.inputPlaceholder = "请输入消息并回车。";
          this.resetEscapeInterrupt();
        }
        this.renderStatus();
        this.renderTasks();
        this.renderInput();
        break;
      case "session.done":
        this.localSubmitPending = false;
        this.running = false;
        this.applySnapshot(event.payload);
        break;
      case "team.started":
      case "team.frame.snapshot":
      case "team.loop.iteration":
      case "team.loop.recovery":
      case "team.agent.started":
      case "team.agent.status":
      case "team.artifact.created":
      case "team.review.required":
      case "team.waiting_for_user":
        this.applyTeamSnapshot(event.payload);
        break;
      case "team.completed":
      case "team.interrupted_by_user":
        this.localSubmitPending = false;
        this.applyTeamSnapshot(event.payload);
        break;
      case "team.dialogue.appended":
        this.appendTeamDialogueEntry(event.payload);
        break;
      case "team.agent.message":
        this.appendTeamAgentDelta(event.payload);
        break;
      case "permission_requested":
        void this.showPermission(event.payload);
        break;
    }
    this.requestRender();
  }

  private handleBackendDisconnect(reason: string): void {
    this.localSubmitPending = false;
    this.running = false;
    this.inputEnabled = true;
    this.inputPlaceholder = this.teamMode ? "Backend 已断开，输入 /exit 退出或重启 lumina。" : "Backend 已断开，输入 /exit 退出或重启 lumina。";
    this.taskLines.push(`backend: ${reason}`);
    this.renderTasks(true);
    this.renderStatus(undefined, true);
    this.renderInput();
    this.requestRender(true);
  }

  private applySnapshot(snapshot: any): void {
    this.sessionID = snapshot.session_id || this.sessionID;
    this.applyFrame(snapshot.frame);
    if (Array.isArray(snapshot.teams) && snapshot.teams.length > 0) {
      this.applyTeamSnapshot(snapshot.teams[snapshot.teams.length - 1]);
    }
  }

  private applyFrame(frame: any): void {
    if (!frame) return;
    this.lastFrame = frame;
    this.inputEnabled = frame.input_enabled !== false;
    this.inputPlaceholder = frame.input_placeholder || "请输入消息并回车。";
    this.applyLocalSubmitLock();
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

  private applyTeamSnapshot(snapshot: any): void {
    if (!snapshot) return;
    const source = snapshot.team_session_id ? snapshot : snapshot.payload || snapshot;
    if (!source.team_session_id && !source.team_mode) return;
    this.teamMode = true;
    this.teamSessionID = source.team_session_id || this.teamSessionID;
    this.activeTeamName = source.active_team_name || source.active_team_id || this.activeTeamName;
    this.teamLoopIteration = Number(source.team_loop_iteration || 0);
    this.running = Boolean(source.running);
    this.inputEnabled = source.input_enabled !== false;
    this.inputPlaceholder = source.input_placeholder || "请输入 Team 消息并回车。";
    this.applyLocalSubmitLock();
    this.teamDialogueEntries = Array.isArray(source.team_dialogue_entries) ? source.team_dialogue_entries : this.teamDialogueEntries;
    this.teamActivityRows = Array.isArray(source.team_activity_rows) ? source.team_activity_rows : this.teamActivityRows;
    this.teamArtifacts = Array.isArray(source.team_artifacts) ? source.team_artifacts : this.teamArtifacts;
    if (Object.prototype.hasOwnProperty.call(source, "team_gate_status")) {
      this.teamGateStatus = source.team_gate_status || {};
    }
    if (Object.prototype.hasOwnProperty.call(source, "team_contract")) {
      this.teamContract = source.team_contract || null;
    }
    if (Object.prototype.hasOwnProperty.call(source, "team_gate_verdicts")) {
      this.teamGateVerdicts = source.team_gate_verdicts || {};
    }
    if (!this.running) {
      this.localSubmitPending = false;
      this.inputEnabled = true;
      this.resetEscapeInterrupt();
      this.teamStreamingText.clear();
    }
    this.renderTranscript();
    this.renderTasks();
    this.renderStatus();
    this.renderInput();
  }

  private applyLocalSubmitLock(): void {
    if (!this.localSubmitPending) return;
    this.running = true;
    this.inputEnabled = false;
    this.inputPlaceholder = this.teamMode ? "Team is working..." : "Agent is responding...";
  }

  private renderTranscript(force = false): void {
    if (!force && this.quietScrollActive()) {
      this.transcriptRenderPending = true;
      this.deferredRenderPending = true;
      this.schedulePaneHoldFlush();
      return;
    }
    const previousScroll = getPaneScroll(this.transcript);
    const shouldFollow = this.transcriptFollow || isPaneAtBottom(this.transcript);
    const content = buildTranscriptContent({
      teamMode: this.teamMode,
      transcriptEntries: this.transcriptEntries,
      teamDialogueEntries: this.teamDialogueEntries,
      teamStreamingText: this.teamStreamingText,
      theme: tuiTheme,
    });
    if (!force && content === this.transcriptContent && !shouldFollow) {
      return;
    }
    if (content !== this.transcriptContent) {
      this.transcript.setContent(content);
      this.transcriptContent = content;
    }
    this.suppressTranscriptScroll = true;
    if (shouldFollow) {
      this.transcript.setScrollPerc(100);
      this.transcriptFollow = true;
    } else {
      this.transcript.scrollTo(previousScroll);
    }
    this.suppressTranscriptScroll = false;
    this.requestRender();
  }

  private appendTeamDialogueEntry(entry: any): void {
    if (!entry || typeof entry !== "object") return;
    const id = String(entry.id || "");
    if (id && this.teamDialogueEntries.some((existing) => existing.id === id)) {
      return;
    }
    if (entry.from_agent) {
      this.teamStreamingText.delete(String(entry.from_agent));
    }
    this.teamDialogueEntries.push(entry);
    this.renderTranscript();
    this.renderTasks();
    this.renderStatus();
    this.renderInput();
  }

  private appendTeamAgentDelta(payload: any): void {
    if (!payload || typeof payload !== "object") return;
    const agentID = String(payload.agent_id || "");
    const delta = String(payload.delta || payload.content || "");
    if (!agentID || !delta) return;
    this.teamStreamingText.set(agentID, (this.teamStreamingText.get(agentID) || "") + delta);
    this.renderTranscript();
  }

  private renderTasks(force = false): void {
    if (!force && this.quietScrollActive()) {
      this.tasksRenderPending = true;
      this.deferredRenderPending = true;
      this.schedulePaneHoldFlush();
      return;
    }
    const previousScroll = getPaneScroll(this.tasks);
    const shouldFollow = this.tasksFollow || isPaneAtBottom(this.tasks);
    const content = buildTasksContent({
      teamMode: this.teamMode,
      running: this.running,
      spinnerFrame: spinnerFrames[this.spinner % spinnerFrames.length],
      spinnerDots: ".".repeat((this.spinner % 3) + 1),
      teamLoopIteration: this.teamLoopIteration,
      teamActivityRows: this.teamActivityRows,
      teamArtifacts: this.teamArtifacts,
      teamContract: this.teamContract,
      teamGateVerdicts: this.teamGateVerdicts,
      taskLines: this.taskLines,
    });
    if (!force && content === this.tasksContent && !shouldFollow) {
      return;
    }
    if (content !== this.tasksContent) {
      this.tasks.setContent(content);
      this.tasksContent = content;
    }
    this.suppressTasksScroll = true;
    if (shouldFollow) {
      this.tasks.setScrollPerc(100);
      this.tasksFollow = true;
    } else {
      this.tasks.scrollTo(previousScroll);
    }
    this.suppressTasksScroll = false;
    this.requestRender();
  }

  private renderStatus(frame?: any, force = false): void {
    if (!force && this.quietScrollActive()) {
      this.statusRenderPending = true;
      this.deferredRenderPending = true;
      this.schedulePaneHoldFlush();
      return;
    }
    frame = frame || this.lastFrame;
    const header = buildHeaderContent({
      teamMode: this.teamMode,
      activeTeamName: this.activeTeamName,
      running: this.running,
      spinnerFrame: spinnerFrames[this.spinner % spinnerFrames.length],
      theme: tuiTheme,
    });
    const status = buildStatusContent({
      teamMode: this.teamMode,
      activeTeamName: this.activeTeamName,
      teamLoopIteration: this.teamLoopIteration,
      teamActivityRows: this.teamActivityRows,
      teamGateStatus: this.teamGateStatus,
      teamContract: this.teamContract,
      teamGateVerdicts: this.teamGateVerdicts,
      frame,
    });
    if (header !== this.headerContent) {
      this.header.setContent(header);
      this.headerContent = header;
    }
    if (status !== this.statusContent) {
      this.status.setContent(status);
      this.statusContent = status;
    }
    this.requestRender();
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
    this.input.setLabel(this.teamMode ? " ● Team 输入 " : " ● 输入 ");
    const runes = Array.from(this.inputBuffer);
    this.inputCursor = Math.max(0, Math.min(this.inputCursor, runes.length));
    const before = runes.slice(0, this.inputCursor).join("");
    const after = runes.slice(this.inputCursor).join("");
    const prompt = this.inputEnabled && !this.running ? "❯" : "·";
    if (this.inputBuffer === "") {
      this.input.setContent(`${prompt} ▌ ${this.inputPlaceholder}`);
      this.inputScroll = 0;
      this.input.scrollTo(0);
      return;
    }
    this.input.setContent(`${prompt} ${before}▌${after}`);
    this.scrollInputToCursor(before);
  }

  private scrollInputToCursor(beforeCursor: string): void {
    const cursorLine = beforeCursor.split("\n").length - 1;
    const visibleHeight = Math.max(1, Number(this.input.height || 0) - 2);
    if (cursorLine < this.inputScroll) {
      this.inputScroll = cursorLine;
    } else if (cursorLine >= this.inputScroll + visibleHeight) {
      this.inputScroll = Math.max(0, cursorLine - visibleHeight + 1);
    }
    this.input.scrollTo(this.inputScroll);
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
    this.requestRender(true);
  }

  private insertPastedText(text: string): void {
    if (!this.inputEnabled || this.running || !text) return;
    this.resetHistoryBrowse();
    this.hideMenu();
    const normalized = text.replace(/\r\n/g, "\n").replace(/\r/g, "\n");
    const insert = Array.from(normalized).filter((char) => char === "\n" || char === "\t" || isPrintableInput(char));
    if (insert.length === 0) return;
    const runes = Array.from(this.inputBuffer);
    runes.splice(this.inputCursor, 0, ...insert);
    this.inputBuffer = runes.join("");
    this.inputCursor += insert.length;
    this.renderInput();
    this.requestRender(true);
  }

  private moveInputCursor(delta: number): void {
    this.inputCursor = Math.max(0, Math.min(Array.from(this.inputBuffer).length, this.inputCursor + delta));
    this.renderInput();
    this.requestRender(true);
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
    this.requestRender(true);
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
    this.requestRender(true);
  }

  private async showPermission(payload: any): Promise<void> {
    const source = payload?.agent_display ? `${payload.agent_display} 请求执行` : "需要权限确认";
    const details = formatPermissionPrompt(payload);
    const choices = [
      { label: "允许一次", decision: "once" },
      { label: "总是允许", decision: "always" },
      { label: "拒绝", decision: "deny" },
    ];
    let selected = 0;
    const render = () => {
      const choiceLine = choices
        .map((choice, index) => {
          const mark = index === selected ? "■" : "□";
          const styleOpen = index === selected ? `{${tuiTheme.selectionBg}-bg}{${tuiTheme.selectionFg}-fg}` : "";
          const styleClose = index === selected ? `{/${tuiTheme.selectionFg}-fg}{/${tuiTheme.selectionBg}-bg}` : "";
          return `${styleOpen}${mark} ${escapeBlessedTags(choice.label)}${styleClose}`;
        })
        .join("   ");
      this.modal.setContent(
        `{${tuiTheme.warning}-fg}${escapeBlessedTags(source)}{/${tuiTheme.warning}-fg}\n` +
          `${choiceLine}\n` +
          `{${tuiTheme.muted}-fg}←/→ 选择，Enter 确认，Esc 拒绝；正文可上下滚动。{/${tuiTheme.muted}-fg}\n\n` +
          escapeBlessedTags(details),
      );
      this.requestRender(true);
    };
    const resolve = async (decision: string) => {
      this.closeModal();
      await this.rpc.call("permission.resolve", { session_id: this.sessionID, team_session_id: payload.team_session_id, request_id: payload.request_id, decision });
      this.requestRender(true);
    };
    render();
    this.modal.scrollTo(0);
    this.modal.show();
    this.modal.focus();
    this.requestRender(true);
    this.setModalHandlers({
      left: () => {
        selected = (selected + choices.length - 1) % choices.length;
        this.modal.scrollTo(0);
        render();
      },
      right: () => {
        selected = (selected + 1) % choices.length;
        this.modal.scrollTo(0);
        render();
      },
      enter: () => void resolve(choices[selected].decision),
      return: () => void resolve(choices[selected].decision),
      escape: () => void resolve("deny"),
    });
  }

  private confirmModal(title: string, message: string): Promise<boolean> {
    this.modal.setContent(`{${tuiTheme.warning}-fg}${escapeBlessedTags(title)}{/${tuiTheme.warning}-fg}\n\n${escapeBlessedTags(message)}\n\n[y] 确认   [n/esc] 取消`);
    this.modal.scrollTo(0);
    this.modal.show();
    this.modal.focus();
    this.requestRender(true);
    return new Promise((resolve) => {
      const done = (value: boolean) => {
        this.closeModal();
        resolve(value);
      };
      this.setModalHandlers({
        y: () => done(true),
        n: () => done(false),
        escape: () => done(false),
      });
    });
  }

  private setModalHandlers(handlers: Record<string, () => void>): void {
    this.modalKeyHandlers.clear();
    for (const [key, handler] of Object.entries(handlers)) {
      this.modalKeyHandlers.set(key, handler);
      this.modalKeyHandlers.set(key.toLowerCase(), handler);
    }
  }

  private closeModal(): void {
    this.modalKeyHandlers.clear();
    this.modal.hide();
    this.input.focus();
    this.requestRender(true);
  }

  private setInput(value: string): void {
    this.inputBuffer = value;
    this.inputCursor = Array.from(value).length;
    this.renderInput();
    this.input.focus();
    this.requestRender(true);
  }

  private hideMenu(): void {
    this.menu.hide();
    this.menuMode = null;
    this.input.focus();
    this.requestRender(true);
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

function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return "0 B";
  if (bytes < 1024) return `${Math.round(bytes)} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let value = bytes;
  for (const unit of units) {
    value /= 1024;
    if (value < 1024) return `${value.toFixed(1)} ${unit}`;
  }
  return `${(value / 1024).toFixed(1)} PB`;
}
