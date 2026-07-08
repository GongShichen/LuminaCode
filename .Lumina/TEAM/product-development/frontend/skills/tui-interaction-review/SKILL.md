---
name: TUI Interaction Review
description: Review command-line or terminal interaction behavior and state transitions.
when-to-use: Use for input, completion, menus, modals, scrolling, streaming output, permissions, or active-run UX.
user-invocable: false
context: inline
---

Check:

- Input editing, cursor movement, selection, paste, history, multiline text, and IME behavior.
- Completion, nested menus, command discovery, keyboard shortcuts, and escape/cancel behavior.
- Modal focus, confirmation flows, permission routing, destructive-action clarity, and error recovery.
- Scroll independence during active runs and clear rules for auto-scroll versus user-controlled scroll.
- Streaming and progress states that do not duplicate text, collapse distinct speakers, or hide final results.
- No raw escape sequences, JSON, tool payloads, stack traces, or hidden reasoning in the main conversation unless intentionally exposed.
