export const bracketedPasteStart = "\x1b[200~";
export const bracketedPasteEnd = "\x1b[201~";
export const strayBracketedPasteStart = "[200~";
export const strayBracketedPasteEnd = "[201~";

export type CursorSequence = { name: "up" | "down" | "right" | "left" | "home" | "end"; length: number };
export type TerminalWheelSequence = { direction: "up" | "down"; x?: number; y?: number };
export type TerminalControlSequence = { length: number; wheel?: TerminalWheelSequence };

export function isPrintableInput(value: string | undefined): value is string {
  if (!value) return false;
  for (const char of Array.from(value)) {
    if (/[\u0000-\u001f\u007f]/u.test(char)) return false;
  }
  return true;
}

export function setBracketedPaste(input: any, enabled: boolean): void {
  const sequence = enabled ? "\x1b[?2004h" : "\x1b[?2004l";
  try {
    const output = input?.output || process.stdout;
    output.write?.(sequence);
  } catch {
    // Best effort; plain multiline paste detection still handles terminals that ignore this.
  }
}

export function parseBracketedPaste(rest: string): { content: string; length: number } | null {
  if (!rest.startsWith(bracketedPasteStart)) return null;
  const endIndex = rest.indexOf(bracketedPasteEnd, bracketedPasteStart.length);
  if (endIndex < 0) {
    return null;
  }
  return {
    content: rest.slice(bracketedPasteStart.length, endIndex),
    length: endIndex + bracketedPasteEnd.length,
  };
}

export function looksLikePlainMultilinePaste(rest: string): boolean {
  if (rest === "\r" || rest === "\n" || rest === "\r\n") return false;
  if (!/[\r\n]/.test(rest)) return false;
  return Array.from(rest.replace(/[\r\n]/g, "")).some((char) => isPrintableInput(char));
}

export function parseTerminalControlSequence(rest: string): TerminalControlSequence | null {
  if (rest.startsWith("\x1b[M")) {
    const chars = Array.from(rest);
    if (chars.length >= 6) {
      const sequence = chars.slice(0, 6).join("");
      return parseX10Mouse(sequence, sequence.length);
    }
    return { length: rest.length };
  }
  if (rest.startsWith("[M")) {
    const chars = Array.from(rest);
    if (chars.length >= 5) {
      const sequence = `\x1b${chars.slice(0, 5).join("")}`;
      return parseX10Mouse(sequence, chars.slice(0, 5).join("").length);
    }
    return { length: rest.length };
  }
  const sgrMouse = /^\x1b\[<\d+(?:;\d+){0,2}[mM]/.exec(rest);
  if (sgrMouse) return parseSGRMouse(sgrMouse[0], sgrMouse[0].length);
  const straySGRMouse = /^\[<\d+(?:;\d+){0,2}[mM]/.exec(rest);
  if (straySGRMouse) return parseSGRMouse(`\x1b${straySGRMouse[0]}`, straySGRMouse[0].length);
  const urxvtMouse = /^\x1b\[\d+(?:;\d+){2}M/.exec(rest);
  if (urxvtMouse) return parseUrxvtMouse(urxvtMouse[0], urxvtMouse[0].length);
  const strayUrxvtMouse = /^\[\d+(?:;\d+){2}M/.exec(rest);
  if (strayUrxvtMouse) return parseUrxvtMouse(`\x1b${strayUrxvtMouse[0]}`, strayUrxvtMouse[0].length);
  if (rest.startsWith("\x1b[I") || rest.startsWith("\x1b[O")) {
    return { length: 3 };
  }
  if (rest.startsWith("[I") || rest.startsWith("[O")) {
    return { length: 2 };
  }
  return null;
}

function parseSGRMouse(sequence: string, length: number): TerminalControlSequence {
  const match = /^\x1b\[<(\d+);(\d+);(\d+)([mM])/.exec(sequence);
  if (!match) return { length };
  const button = Number(match[1]);
  if (((button >> 6) & 1) !== 1) return { length };
  return {
    length,
    wheel: {
      direction: button & 1 ? "down" : "up",
      x: Number(match[2]),
      y: Number(match[3]),
    },
  };
}

function parseX10Mouse(sequence: string, length: number): TerminalControlSequence {
  const chars = Array.from(sequence);
  if (chars.length < 6) return { length };
  const button = chars[3].charCodeAt(0) - 32;
  if (((button >> 6) & 1) !== 1) return { length };
  return {
    length,
    wheel: {
      direction: button & 1 ? "down" : "up",
      x: chars[4].charCodeAt(0) - 32,
      y: chars[5].charCodeAt(0) - 32,
    },
  };
}

function parseUrxvtMouse(sequence: string, length: number): TerminalControlSequence {
  const match = /^\x1b\[(\d+);(\d+);(\d+)M/.exec(sequence);
  if (!match) return { length };
  const button = Number(match[1]) - 32;
  if (((button >> 6) & 1) !== 1) return { length };
  return {
    length,
    wheel: {
      direction: button & 1 ? "down" : "up",
      x: Number(match[2]),
      y: Number(match[3]),
    },
  };
}

export function parseCursorSequence(rest: string): CursorSequence | null {
  const applicationCursor: Record<string, CursorSequence["name"]> = {
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
  const names: Record<string, CursorSequence["name"]> = {
    A: "up",
    B: "down",
    C: "right",
    D: "left",
    H: "home",
    F: "end",
  };
  return { name: names[key], length: csi[0].length };
}
