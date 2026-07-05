---
name: PDF Work
description: Read, create, or review PDF files when layout fidelity matters, using rendered page inspection before delivery.
when-to-use: When the user asks to inspect, generate, edit, extract from, or validate a PDF and visual layout or pagination matters.
user-invocable: true
disable-model-invocation: false
context: inline
---

Treat PDFs as visual artifacts. Prefer rendering pages to images and inspecting the result over relying only on extracted text.

Recommended workflow:
- Use `pdftoppm` or an equivalent renderer to convert pages to PNG when available.
- Use structured libraries such as `pypdf`, `pdfplumber`, or `reportlab` for extraction and generation instead of ad hoc byte edits.
- Keep temporary render outputs under `tmp/pdfs/` and final artifacts under `output/pdf/` unless the user gives another path.
- Re-render after meaningful edits and verify margins, clipping, tables, images, page numbers, headers, and footers.

If a dependency is missing, state the exact missing tool and the command needed to install it. Do not deliver a generated PDF as complete until the latest render has been visually checked or you have clearly explained why rendering was not possible.
