import { execFileSync } from "node:child_process";

export type TuiTheme = {
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
};

export function createTheme(): TuiTheme {
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
