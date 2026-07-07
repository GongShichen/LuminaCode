import {
  bracketedPasteEnd,
  bracketedPasteStart,
  looksLikePlainMultilinePaste,
  parseBracketedPaste,
  parseCursorSequence,
  parseTerminalControlSequence,
  strayBracketedPasteEnd,
  strayBracketedPasteStart,
} from "./input";

export type RawInputHandlers = {
  insertPastedText(text: string): void;
  handleWheel(direction: "up" | "down", data: { x?: number; y?: number }): void;
  handleKey(ch: string | undefined, key: any): Promise<void>;
  exit(): void;
};

export class RawInputDispatcher {
  private bracketedPasteBuffer: string | null = null;

  constructor(private handlers: RawInputHandlers) {}

  async handle(text: string): Promise<void> {
    let i = 0;
    while (i < text.length) {
      const rest = text.slice(i);
      if (this.bracketedPasteBuffer !== null) {
        const endIndex = rest.indexOf(bracketedPasteEnd);
        if (endIndex < 0) {
          this.bracketedPasteBuffer += rest;
          return;
        }
        this.handlers.insertPastedText(this.bracketedPasteBuffer + rest.slice(0, endIndex));
        this.bracketedPasteBuffer = null;
        i += endIndex + bracketedPasteEnd.length;
        continue;
      }
      if (rest.startsWith(bracketedPasteEnd)) {
        i += bracketedPasteEnd.length;
        continue;
      }
      if (rest.startsWith(strayBracketedPasteEnd)) {
        i += strayBracketedPasteEnd.length;
        continue;
      }
      const paste = parseBracketedPaste(rest);
      if (paste) {
        this.handlers.insertPastedText(paste.content);
        i += paste.length;
        continue;
      }
      if (rest.startsWith(bracketedPasteStart)) {
        this.bracketedPasteBuffer = rest.slice(bracketedPasteStart.length);
        return;
      }
      if (rest.startsWith(strayBracketedPasteStart)) {
        this.bracketedPasteBuffer = rest.slice(strayBracketedPasteStart.length);
        return;
      }
      if (looksLikePlainMultilinePaste(rest)) {
        this.handlers.insertPastedText(rest);
        return;
      }
      const terminalEvent = parseTerminalControlSequence(rest);
      if (terminalEvent) {
        if (terminalEvent.wheel) {
          this.handlers.handleWheel(terminalEvent.wheel.direction, { x: terminalEvent.wheel.x, y: terminalEvent.wheel.y });
        }
        i += terminalEvent.length;
        continue;
      }
      const cursor = parseCursorSequence(rest);
      if (cursor) {
        await this.handlers.handleKey(undefined, { name: cursor.name });
        i += cursor.length;
        continue;
      }
      if (rest.startsWith("\x1b[3~")) {
        await this.handlers.handleKey(undefined, { name: "delete" });
        i += 4;
        continue;
      }
      if (rest.startsWith("\x1b[A")) {
        await this.handlers.handleKey(undefined, { name: "up" });
        i += 3;
        continue;
      }
      if (rest.startsWith("\x1b[B")) {
        await this.handlers.handleKey(undefined, { name: "down" });
        i += 3;
        continue;
      }
      if (rest.startsWith("\x1b[C")) {
        await this.handlers.handleKey(undefined, { name: "right" });
        i += 3;
        continue;
      }
      if (rest.startsWith("\x1b[D")) {
        await this.handlers.handleKey(undefined, { name: "left" });
        i += 3;
        continue;
      }
      if (rest.startsWith("\x1b[H") || rest.startsWith("\x1b[1~")) {
        await this.handlers.handleKey(undefined, { name: "home" });
        i += rest.startsWith("\x1b[1~") ? 4 : 3;
        continue;
      }
      if (rest.startsWith("\x1b[F") || rest.startsWith("\x1b[4~")) {
        await this.handlers.handleKey(undefined, { name: "end" });
        i += rest.startsWith("\x1b[4~") ? 4 : 3;
        continue;
      }

      const char = Array.from(rest)[0] || "";
      i += char.length || 1;
      switch (char) {
        case "\x03":
          this.handlers.exit();
          return;
        case "\r":
        case "\n":
          await this.handlers.handleKey(undefined, { name: "enter" });
          break;
        case "\t":
          await this.handlers.handleKey(undefined, { name: "tab" });
          break;
        case "\x1b":
          await this.handlers.handleKey(undefined, { name: "escape" });
          break;
        case "\x7f":
        case "\b":
          await this.handlers.handleKey(undefined, { name: "backspace" });
          break;
        case "\x01":
          await this.handlers.handleKey(undefined, { name: "C-a" });
          break;
        case "\x05":
          await this.handlers.handleKey(undefined, { name: "C-e" });
          break;
        case "\x15":
          await this.handlers.handleKey(undefined, { name: "C-u" });
          break;
        default:
          await this.handlers.handleKey(char, { name: "char" });
          break;
      }
    }
  }
}
