---
name: Terminal UI Design
description: Design polished text-first and terminal interfaces with stable layout and readable interaction states.
when-to-use: Use when changing CLI/TUI layout, prompts, colors, activity display, streaming output, menus, forms, logs, or command interaction.
user-invocable: false
context: inline
---

Design for repeated real work:

- Clear hierarchy, concise labels, predictable alignment, and restrained color.
- Stable layout across narrow, wide, small-height, and high-DPI terminals.
- Input that supports editing, history, cancellation, completion, paste, IME, and long text where relevant.
- Scroll behavior that lets users inspect history while work continues.
- Progress and active states that communicate liveness without distracting motion.
- Accessible contrast, no reliance on color alone, and graceful degradation for plain terminals.
- Separation between user-facing content, diagnostics, logs, and raw protocol/tool payloads.
