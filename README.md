# oopa — magic todo

A tiny, single-binary clone of goblin.tools' **Magic Todo**. Magically breaks
overwhelming tasks into small, concrete steps — recursively, until they're
doable. It is a local-first, privacy-focused tool that works with any
OpenAI-compatible LLM server (like LM Studio, Ollama, or vLLM).

Runs two ways from the **same binary**, sharing one JSON store and one
breakdown engine:

- `oopa` — full-screen terminal UI
- `oopa web` — local web UI served from the same binary

## Setup

1. Install [LM Studio](https://lmstudio.ai), load a model (e.g.
   `qwen2.5-7b-instruct` or any chat model), and start its local server
   (the little `⇄` tab → Start Server). It listens on
   `http://localhost:1234/v1`.
2. Build:

   ```sh
   make build
   ```

## Run

```sh
./oopa                 # TUI mode
./oopa web             # web UI at http://127.0.0.1:7777
./oopa web 127.0.0.1:9000 # custom local port
```

Open the printed URL in a browser and add a task, then click **magic** on
any task or subtask to break it into smaller steps. The web UI includes a
light/dark theme switch and an LM Studio settings panel.

### Environment

| var             | default                          | meaning                       |
|-----------------|----------------------------------|-------------------------------|
| `OOPA_LLM_URL`  | LLM base URL (auto-detected by default)    |
| `OOPA_LLM_MODEL`| force a model id; otherwise best non-reasoning model |
| `OOPA_LLM_API_KEY` | (none)                        | optional bearer token         |
| `OOPA_STORE`    | `~/.oopa-todo.json`              | where your task tree is saved |
| `OOPA_WEB_ADDR` | `127.0.0.1:7777`                 | companion web UI address in TUI mode |

## TUI keys

```
↑/↓ or j/k          move between tasks
space / Enter       mark complete/incomplete
a                   add a top-level task
A                   add a subtask under the selected task
e                   edit selected task
m                   break selected task into subtasks
d / Delete          delete selected task and its subtree
M                   choose the LLM model
u                   set the LLM URL
S                   settings overview
X                   export JSON or Markdown
r                   reload from disk
?                   help
q                   save and quit
```

## Layout

```
main.go                  entrypoint; builds engine + picks CLI or web
internal/todo/           recursive task tree + JSON store
internal/llm/            LM Studio (OpenAI-compatible) client
internal/breakdown/      recursive Magic Todo breakdown
internal/tui/            full-screen terminal UI
internal/web/            HTTP server + embedded HTML UI
```

## Notes

- Breakdowns cap at depth 3 and 6 children per level to keep the tree
  sane — same idea as goblin.tools, where you decide when to keep
  breaking. Click `magic` again on a leaf to dig deeper.
- No telemetry, no network calls except to your own local LM Studio.
