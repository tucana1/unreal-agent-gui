---
name: web-research
description: Search the web and read pages, PDFs, and online documents. Use for anything current, factual, or needing citations.
---
# Web research

Run these with Bash. `$UAG` is the GUI binary; always quote it.

- `"$UAG" search -n 8 "query"` prints numbered results (title, URL, snippet). It uses Brave Search when BRAVE_API_KEY is set, otherwise DuckDuckGo.
- `"$UAG" fetch "https://example.com/page"` prints the readable text of an HTML page, PDF, or text/JSON URL, with links as markdown. Output stops at 20000 characters; continue with `-offset N` or change the cap with `-max N`.

## Method
1. Run several searches with different phrasings as parallel Bash calls in the same turn.
2. Fetch the most promising sources in parallel. Prefer primary sources: official documentation, papers, filings, datasets, and original reporting over summaries.
3. Cross-check important claims across independent sources. Note publication dates and disagreements.
4. Answer with inline citations as markdown links to pages you actually read. Say what you could not verify.

## When things fail
- Rate-limited search: wait a few seconds and retry, rephrase, or query a site's own search/API with curl. Suggest the user add BRAVE_API_KEY in Settings.
- JavaScript-only pages that fetch returns empty: look for the site's API, RSS feed, `?format=json`, a print view, or an archived copy at `https://web.archive.org/web/<url>`.
- Files to keep (PDFs, datasets, images): `curl -L -o <name> <url>` into the workspace, then use the documents skill or ViewImage.
- For long investigations, keep notes in a markdown file in the workspace and cite from it.
