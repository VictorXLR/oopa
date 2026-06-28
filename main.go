// Command oopa is a tiny goblin.tools-style Magic Todo app.
//
// It runs as a full-screen terminal UI by default, or a small web
// server. The same todo store and breakdown engine back every surface,
// so a task added in one shows up in the others. The todo list is kept
// as a single JSON file and can be exported as JSON or Markdown.
//
// Usage:
//
//	oopa                       full-screen terminal UI (default)
//	oopa tui                   same, explicitly
//	oopa web [addr]            web UI (default 127.0.0.1:7777)
//	oopa web --addr :9000      choose listen port
//	oopa export [path]         dump the store; .md -> Markdown, else JSON
//	                           (stdout when no path given)
//
// Environment:
//
//	LMSTUDIO_URL    LM Studio base URL (auto-detected by default)
//	LMSTUDIO_MODEL  force a model id; otherwise best non-reasoning model
//	LMSTUDIO_API_KEY optional bearer token
//	OOPA_STORE      path to store JSON (default ~/.oopa-todo.json)
//	OOPA_WEB_ADDR   companion web UI address in TUI mode
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/loopy/oopa/internal/breakdown"
	"github.com/loopy/oopa/internal/config"
	"github.com/loopy/oopa/internal/export"
	"github.com/loopy/oopa/internal/llm"
	"github.com/loopy/oopa/internal/todo"
	"github.com/loopy/oopa/internal/tui"
	"github.com/loopy/oopa/internal/web"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	storePath := envOr("OOPA_STORE", defaultStorePath())

	root, err := todo.Load(storePath)
	if err != nil {
		return fmt.Errorf("loading store: %w", err)
	}

	// export needs neither the LLM nor the engine.
	if len(args) > 0 && args[0] == "export" {
		return runExport(root, args[1:])
	}

	// Settings precedence: explicit env var > persisted config > auto-probe.
	cfgPath := config.DefaultPath()
	cfg, _ := config.Load(cfgPath)
	baseURL := envOr("LMSTUDIO_URL", cfg.BaseURL)
	model := envOr("LMSTUDIO_MODEL", cfg.Model)

	client := llm.New(baseURL, model, os.Getenv("LMSTUDIO_API_KEY"))
	llmOK := true
	if err := client.PickModel(); err != nil {
		llmOK = false
		fmt.Fprintf(os.Stderr, "warn: could not reach LM Studio (%v)\n", err)
		fmt.Fprintln(os.Stderr, "      magic breakdown will fail until LM Studio is running and a model is loaded.")
		fmt.Fprintln(os.Stderr, "      set the endpoint in-app (settings / url command, or the web settings panel),")
		fmt.Fprintln(os.Stderr, "      via LMSTUDIO_URL, or run it on one of these auto-probed hosts:")
		for _, u := range llm.DefaultBaseURLs {
			fmt.Fprintf(os.Stderr, "        %s\n", u)
		}
	}
	engine := breakdown.New(client)

	// Warm the model up in the background so the first magic isn't slowed
	// by a cold model load.
	if llmOK {
		go client.WarmUp()
	}

	// Shared store guards: the TUI and the companion web server mutate the
	// same tree under one mutex, persisted by one debounced saver.
	mu := &sync.Mutex{}
	saver := todo.NewSaver(storePath)

	if len(args) == 0 || args[0] == "tui" {
		// Run the web server alongside the TUI on the same data.
		webAddr := envOr("OOPA_WEB_ADDR", "127.0.0.1:7777")
		go func() {
			// Non-fatal for the TUI if the port is busy; just no web UI.
			_ = web.New(root, storePath, engine, client, cfgPath, mu, saver).Listen(webAddr)
		}()
		return tui.New(root, storePath, engine, client, cfgPath, webAddr, mu, saver).Run()
	}

	switch args[0] {
	case "web":
		fs := flag.NewFlagSet("web", flag.ExitOnError)
		addr := fs.String("addr", "127.0.0.1:7777", "listen address")
		_ = fs.Parse(args[1:])
		if len(fs.Args()) > 0 {
			*addr = fs.Arg(0)
		}
		fmt.Printf("magic-todo web on %s\n", displayURL(*addr))
		return web.New(root, storePath, engine, client, cfgPath, mu, saver).Listen(*addr)
	default:
		return fmt.Errorf("unknown subcommand %q (try `web` or `export`)", args[0])
	}
}

// runExport writes the store to a path or stdout. Format is chosen by
// file extension: .md/.markdown -> Markdown checklist, otherwise JSON.
func runExport(root *todo.Root, args []string) error {
	path := ""
	if len(args) > 0 {
		path = args[0]
	}
	if path == "" {
		b, err := export.JSON(root)
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}
	switch export.PickFormat(path) {
	case "markdown":
		return os.WriteFile(path, []byte(export.Markdown(root)+"\n"), 0o644)
	default:
		b, err := export.JSON(root)
		if err != nil {
			return err
		}
		return os.WriteFile(path, b, 0o644)
	}
}

func displayURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		if len(addr) > 0 && addr[0] == ':' {
			return "http://127.0.0.1" + addr
		}
		return "http://" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func defaultStorePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".oopa-todo.json"
	}
	return filepath.Join(home, ".oopa-todo.json")
}
