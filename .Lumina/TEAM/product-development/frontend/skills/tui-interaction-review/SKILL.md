---
name: TUI Interaction Review
description: Review terminal interaction behavior and state transitions.
when-to-use: Use for input, menus, modals, scrolling, or active-run UX.
user-invocable: false
context: inline
---

Check:

- Input editing, cursor movement, history, and IME behavior.
- Slash completion and nested menus.
- Modal focus, cancellation, and permission routing.
- Scroll independence during active runs.
- No duplicate text or raw escape sequence leakage.
