# Unreal Agent GUI

A small macOS desktop app for [Unreal Agent](https://github.com/unreallabsai/unreal-agent), the async-first agent harness from Unreal Labs. It is built for research work: web search, reading PDFs and Office files, looking at images, running code, and working with GitHub, all inside a workspace folder whose files you can preview next to the chat.

This repository is a fork of `unreallabsai/unreal-agent`. Everything added lives in [`gui/`](../gui) and `.github/`; upstream files are untouched, so upstream changes merge in cleanly (see [Staying current with upstream](#staying-current-with-upstream)). The upstream README is at [`/README.md`](../README.md).

## What it does

- **Chat with an agent that keeps working in the background.** Each chat runs a harness coordinator in-process. Tool calls run asynchronously, and messages you send while the agent works join the run in progress instead of waiting for it to finish.
- **Web research.** A built-in `web-research` skill gives the agent `search` (DuckDuckGo, or Brave Search when `BRAVE_API_KEY` is set) and `fetch` (readable text from web pages and PDFs). OpenAI and ChatGPT providers can also use the provider-hosted `web_search` tool.
- **Attachments.** Attach, paste, or drop files. They are saved to `uploads/` in the workspace; the agent views images with the harness's `ViewImage` tool and reads documents with the `documents` skill.
- **Document previews.** The Files panel previews Markdown, PDF, images, HTML, CSV, JSON, code, audio, and video, and extracts text from Word, PowerPoint, Excel, and OpenDocument files.
- **GitHub.** A `github` skill drives the `gh` CLI. The sidebar shows whether `gh` is logged in.
- **Any provider the harness supports:** OpenRouter (the default, with `openai/gpt-6-astra`), OpenAI, ChatGPT through an existing Codex CLI login, Fireworks, and Ollama.

Sessions use the harness's own append-only session store, so chats resume after a restart. Skills you put in `<workspace>/.harness/skills/` work as they do with the upstream runner.

## Install

You need macOS 12+, Go 1.27+, and the Xcode command-line tools (`xcode-select --install`).

```sh
git clone https://github.com/tucana1/unreal-agent-gui
cd unreal-agent-gui/gui
make install-app      # builds "Unreal Agent.app" into ~/Applications
```

Or run it without installing: `make run`.

On first launch, Settings opens so you can paste an [OpenRouter API key](https://openrouter.ai/keys). Pick any model in the toolbar; the list comes from the provider.

Useful extras:

- `brew install gh && gh auth login` for GitHub.
- `brew install poppler` for higher-quality PDF text; a pure-Go fallback works without it.
- `BRAVE_API_KEY` (Settings → Connections) for reliable search. DuckDuckGo rate-limits heavy use.

### Other platforms

The native window is macOS-only for now. Elsewhere, `go build` produces a binary that opens the same UI in your browser (`-browser` does this on macOS too). The harness itself supports Linux; Windows is untested.

## Security

The agent runs shell commands **on your computer with your permissions**, not in a sandbox. Point it at a workspace folder you are comfortable with, and review what it does. The skills tell it to ask before destructive or outward-facing actions, but that is guidance, not enforcement.

The app's local API listens only on loopback. It requires a per-install token cookie (`SameSite=Strict`) plus a custom header for state changes, and rejects non-loopback `Host` headers. Workspace files are previewed under a sandbox CSP so agent-written HTML cannot call the API. Settings, including API keys, are stored in `~/Library/Application Support/unreal-agent-gui/config.json` with mode 600.

## How it's built

A single Go binary (about 10 MB) with no runtime dependencies:

| Piece | What it is |
| --- | --- |
| [`gui/chat.go`](../gui/chat.go) | One harness coordinator per chat, built from the public `harness/` packages: local session store, inbox, context builder, Bash/ViewImage/SkillUse tools, and LLM clients. |
| [`gui/server.go`](../gui/server.go) | Loopback HTTP API and a live event stream (SSE) of session items. |
| [`gui/window_darwin.go`](../gui/window_darwin.go) | Native window using the system WebKit through [webview](https://github.com/webview/webview_go), plus a standard macOS menu. |
| [`gui/tools.go`](../gui/tools.go) | `search`, `fetch`, and `read` subcommands that the skills call through Bash. |
| [`gui/skills/`](../gui/skills) | Built-in skills: `web-research`, `documents`, `github`. |
| [`gui/ui/`](../gui/ui) | Plain HTML/CSS/JS, no build step. Vendors [marked](https://github.com/markedjs/marked) and [DOMPurify](https://github.com/cure53/DOMPurify). |

`gui/` is its own Go module with `replace github.com/unreallabsai/unreal-agent => ../`, so it always builds against the harness in this checkout. It imports only upstream's public `harness/` packages, not `cmd/internal`.

## Staying current with upstream

Unreal Agent is moving fast, so the fork is set up to follow it:

- **Automatic:** [`sync-upstream.yml`](workflows/sync-upstream.yml) runs daily (and on demand from the Actions tab). It merges `unreallabsai/unreal-agent` `main`, then builds and tests the GUI against it. If that passes, it pushes the merge; if not, it opens an issue with the failure instead of breaking `main`.
- **Manual:** GitHub's **Sync fork** button works, or:

  ```sh
  git fetch upstream && git merge upstream/main
  cd gui && go mod tidy && make test build
  ```

The GUI's own tests include a drift check that fails when upstream adds an LLM provider the GUI doesn't list yet.

## License

MIT, same as upstream; see [LICENSE](../LICENSE) (Copyright (c) 2026 Unreal Labs). The GUI additions are offered under the same license. Vendored libraries keep their licenses: see [`gui/ui/vendor/LICENSES.txt`](../gui/ui/vendor/LICENSES.txt), and `github.com/webview/webview_go` is MIT. This project is not affiliated with Unreal Labs.
