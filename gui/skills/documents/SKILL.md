---
name: documents
description: Read, analyze, convert, and create documents - PDF, Word, PowerPoint, Excel, OpenDocument, HTML, CSV, and images - including files the user attaches.
---
# Documents

## Reading
- `"$UAG" read <file>` extracts text from .pdf, .docx, .pptx, .xlsx, .odt/.ods/.odp, .html, .rtf/.doc (macOS), and plain text. It stops at 50000 characters; continue with `-offset N`.
- Images, charts, figures, and scanned pages: look at them with ViewImage. To see a PDF page on macOS: `sips -s format png in.pdf --out page.png` (first page) or `qlmanage -t -s 1600 -o . in.pdf`. With poppler installed: `pdftoppm -png -r 110 -f 3 -l 3 in.pdf page`.
- Large CSV/JSON: inspect shape first (`head`, `wc -l`, `python3 -c`, `jq`) before processing everything.

## Creating
- Write reports as Markdown (.md) in the workspace; the GUI renders it. HTML, CSV, PDF, and images preview too.
- Check what is installed before converting: `command -v pandoc soffice libreoffice python3 textutil`.
  - pandoc: `pandoc report.md -o report.docx` (or .pdf if a PDF engine is present, .pptx, .html).
  - macOS without pandoc: `textutil -convert docx report.html` converts HTML/RTF to Word.
  - LibreOffice: `soffice --headless --convert-to pdf file.docx`.
- Charts: python3 with matplotlib if available (save PNG, then check it with ViewImage); otherwise write a self-contained HTML file with inline SVG.
- Do not install packages system-wide without asking. Prefer a virtualenv in the workspace (`python3 -m venv .venv`).

Link every file you create by relative path, like [report.md](report.md).
