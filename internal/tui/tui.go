// Package tui is a tview-based, fully navigable terminal UI for Magic
// Todo, in the spirit of goblin.tools. The task tree is a real TreeView:
// move with the arrow keys (or j/k), and act on the highlighted task with
// single keystrokes — no command line, no task ids to remember.
//
// Keys:
//
//	↑/↓ or j/k   move between tasks
//	→/← or l/h   expand / collapse a task's subtasks
//	space/Enter  toggle done
//	a            add a top-level task
//	A            add a subtask under the selected task
//	e            edit the selected task's title
//	m            magic-break the selected task into subtasks
//	d / Delete   delete the selected task (with confirmation)
//	M            choose the LM Studio model
//	u            set the LM Studio URL
//	S            settings overview
//	x            export the list (JSON/Markdown)
//	r            reload from disk
//	?            help
//	q            save and quit
package tui

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/loopy/oopa/internal/breakdown"
	"github.com/loopy/oopa/internal/config"
	"github.com/loopy/oopa/internal/export"
	"github.com/loopy/oopa/internal/llm"
	"github.com/loopy/oopa/internal/todo"
)

// App holds the TUI's dependencies and runtime state.
type App struct {
	Root    *todo.Root
	Path    string
	Engine  *breakdown.Engine
	LLM     *llm.Client
	CfgPath string
	WebAddr string // address of the companion web server, if running

	App    *tview.Application
	Pages  *tview.Pages
	Tree   *tview.TreeView
	Header *tview.TextView
	Status *tview.TextView
	Saver  *todo.Saver

	statusGen int // bumped on each status message; UI-goroutine only

	mu *sync.Mutex // guards Root; may be shared with the web server
}

// New wires an App. mu and saver may be shared with a web.Server running
// in the same process so both surfaces mutate one store under one lock.
func New(root *todo.Root, path string, engine *breakdown.Engine, client *llm.Client, cfgPath, webAddr string, mu *sync.Mutex, saver *todo.Saver) *App {
	if mu == nil {
		mu = &sync.Mutex{}
	}
	if saver == nil {
		saver = todo.NewSaver(path)
	}
	a := &App{Root: root, Path: path, Engine: engine, LLM: client, CfgPath: cfgPath, WebAddr: webAddr, Saver: saver, mu: mu}
	a.build()
	return a
}

// Run starts the tview event loop. Blocks until the user quits.
func (a *App) Run() error {
	return a.App.SetRoot(a.Pages, true).EnableMouse(true).Run()
}

// build assembles the layout: header, navigable tree, status/help line.
func (a *App) build() {
	a.App = tview.NewApplication()
	a.Pages = tview.NewPages()

	a.Header = tview.NewTextView().
		SetDynamicColors(true).
		SetText(a.headerText())

	rootNode := tview.NewTreeNode("magic todo").
		SetColor(tcell.ColorMediumPurple).
		SetSelectable(false)

	a.Tree = tview.NewTreeView().
		SetRoot(rootNode).
		SetCurrentNode(rootNode).
		SetTopLevel(1) // hide the synthetic root; show top-level tasks flush left
	a.Tree.SetBorder(true).
		SetTitle(" tasks ").
		SetTitleAlign(tview.AlignLeft)

	a.Status = tview.NewTextView().
		SetDynamicColors(true).
		SetText(a.helpLine())

	a.Tree.SetInputCapture(a.onTreeKey)

	layout := tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(a.Header, 1, 0, false).
		AddItem(a.Tree, 0, 1, true).
		AddItem(a.Status, 1, 0, false)
	a.Pages.AddPage("main", layout, true, true)

	a.rebuildTree("")
}

// headerText shows the app name, active model, and companion web URL.
func (a *App) headerText() string {
	model := "[red]no LM Studio[white]"
	if a.LLM != nil {
		_, currentModel := a.LLM.Snapshot()
		if currentModel != "" {
			model = "[green]" + tview.Escape(currentModel) + "[white]"
		}
	}
	web := ""
	if a.WebAddr != "" {
		web = fmt.Sprintf("   [darkgray]web:[white] %s", displayURL(a.WebAddr))
	}
	return fmt.Sprintf(" [::b]magic todo[::-]  ·  break overwhelming tasks into doable steps   [darkgray]model:[white] %s%s", model, web)
}

func (a *App) helpLine() string {
	return " [yellow]↑↓[white] move  [yellow]space[white] done  [yellow]m[white] magic  [yellow]a[white] add  [yellow]A[white] subtask  [yellow]e[white] edit  [yellow]d[white] del  [yellow]M[white] model  [yellow]?[white] help  [yellow]q[white] quit"
}

// refreshHeader redraws the header. Call only on the UI goroutine
// (directly from a key handler, or inside an a.ui closure).
func (a *App) refreshHeader() {
	a.Header.SetText(a.headerText())
}

// rebuildTree re-renders the tree from the store, keeping the highlighted
// task (by id) selected where possible. Pass an id to force selection.
// The whole build holds the store lock so a concurrent web mutation can't
// race the traversal.
func (a *App) rebuildTree(selectID string) {
	if selectID == "" {
		selectID = a.selectedID()
	}
	rootNode := a.Tree.GetRoot()
	rootNode.ClearChildren()

	a.mu.Lock()
	defer a.mu.Unlock()
	tasks := a.Root.Tasks

	if len(tasks) == 0 {
		hint := tview.NewTreeNode("(no tasks yet — press 'a' to add one)").
			SetColor(tcell.ColorGray).
			SetSelectable(false)
		rootNode.AddChild(hint)
		return
	}

	var sel *tview.TreeNode
	for _, t := range tasks {
		n := a.makeNode(t)
		rootNode.AddChild(n)
		if found := findNode(n, selectID); found != nil {
			sel = found
		}
	}
	if sel == nil {
		// Default to the first selectable node.
		sel = firstSelectable(rootNode)
	}
	if sel != nil {
		a.Tree.SetCurrentNode(sel)
	}
}

// makeNode builds a tree node (and its subtree) for a task.
func (a *App) makeNode(t *todo.Task) *tview.TreeNode {
	n := tview.NewTreeNode(nodeLabel(t)).
		SetReference(t).
		SetSelectable(true).
		SetColor(nodeColor(t))
	for _, c := range t.Children {
		n.AddChild(a.makeNode(c))
	}
	return n
}

func nodeLabel(t *todo.Task) string {
	box := "[ ]"
	if t.Done {
		box = "[x]"
	}
	return fmt.Sprintf("%s %s", box, t.Title)
}

func nodeColor(t *todo.Task) tcell.Color {
	if t.Done {
		return tcell.ColorGray
	}
	return tcell.ColorWhite
}

// onTreeKey handles single-key actions while the tree has focus.
func (a *App) onTreeKey(ev *tcell.EventKey) *tcell.EventKey {
	// Vim-style nav in addition to the built-in arrow keys.
	switch ev.Rune() {
	case 'j':
		return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
	case 'k':
		return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
	case 'h':
		return tcell.NewEventKey(tcell.KeyLeft, 0, tcell.ModNone)
	case 'l':
		return tcell.NewEventKey(tcell.KeyRight, 0, tcell.ModNone)
	case ' ':
		a.toggleSelected()
		return nil
	case 'a':
		a.promptAdd("")
		return nil
	case 'A':
		if t := a.selected(); t != nil {
			a.promptAdd(t.ID)
		}
		return nil
	case 'e':
		a.promptEdit()
		return nil
	case 'm':
		a.magicSelected()
		return nil
	case 'd', 'x':
		a.confirmDelete()
		return nil
	case 'M':
		a.cmdModel(nil)
		return nil
	case 'u':
		a.promptURL()
		return nil
	case 'S':
		a.cmdSettings()
		return nil
	case 'X':
		a.promptExport()
		return nil
	case 'r':
		a.reloadFromDisk()
		return nil
	case '?':
		a.showHelp()
		return nil
	case 'q':
		a.quit()
		return nil
	}
	switch ev.Key() {
	case tcell.KeyEnter:
		a.toggleSelected()
		return nil
	case tcell.KeyDelete:
		a.confirmDelete()
		return nil
	case tcell.KeyCtrlC:
		a.quit()
		return nil
	}
	return ev
}

// selected returns the highlighted task, or nil.
func (a *App) selected() *todo.Task {
	n := a.Tree.GetCurrentNode()
	if n == nil {
		return nil
	}
	if t, ok := n.GetReference().(*todo.Task); ok {
		return t
	}
	return nil
}

func (a *App) selectedID() string {
	if t := a.selected(); t != nil {
		return t.ID
	}
	return ""
}

// toggleSelected flips the done flag on the highlighted task. It updates
// just that node in place rather than rebuilding the whole tree.
func (a *App) toggleSelected() {
	n := a.Tree.GetCurrentNode()
	if n == nil {
		return
	}
	t, ok := n.GetReference().(*todo.Task)
	if !ok {
		return
	}
	a.mu.Lock()
	t.Done = !t.Done
	label, color := nodeLabel(t), nodeColor(t)
	saveErr := a.Saver.Save(a.Root)
	a.mu.Unlock()
	n.SetText(label).SetColor(color)
	if saveErr != nil {
		a.setStatus("[red]save failed:[white] " + saveErr.Error())
	}
}

// promptAdd opens an input for a new task. parentID == "" adds a top-level
// task; otherwise the new task is a child of parentID.
func (a *App) promptAdd(parentID string) {
	title := "new top-level task"
	if parentID != "" {
		title = "new subtask"
	}
	a.prompt(title, "", func(text string) {
		nt := todo.New(text)
		a.mu.Lock()
		if parentID == "" {
			a.Root.Add(nt)
		} else if p := a.Root.Find(parentID); p != nil {
			p.AddChild(nt)
		} else {
			a.Root.Add(nt)
		}
		saveErr := a.Saver.Save(a.Root)
		a.mu.Unlock()
		a.rebuildTree(nt.ID)
		if saveErr != nil {
			a.setStatus("[red]save failed:[white] " + saveErr.Error())
		} else {
			a.setStatus("added - press [yellow]m[white] to break it into steps")
		}
		if parentID != "" {
			if n := findNode(a.Tree.GetRoot(), parentID); n != nil {
				n.SetExpanded(true)
			}
		}
	})
}

// promptEdit edits the highlighted task's title.
func (a *App) promptEdit() {
	t := a.selected()
	if t == nil {
		return
	}
	id := t.ID
	a.prompt("edit task", t.Title, func(text string) {
		a.mu.Lock()
		var saveErr error
		if tt := a.Root.Find(id); tt != nil {
			tt.Title = text
			saveErr = a.Saver.Save(a.Root)
		}
		a.mu.Unlock()
		a.rebuildTree(id)
		if saveErr != nil {
			a.setStatus("[red]save failed:[white] " + saveErr.Error())
		}
	})
}

// confirmDelete asks before removing the highlighted task and its subtree.
func (a *App) confirmDelete() {
	t := a.selected()
	if t == nil {
		return
	}
	id := t.ID
	modal := tview.NewModal().
		SetText(fmt.Sprintf("Delete %q and all its subtasks?", t.Title)).
		AddButtons([]string{"Delete", "Cancel"}).
		SetDoneFunc(func(_ int, label string) {
			a.Pages.RemovePage("confirm")
			a.App.SetFocus(a.Tree)
			if label == "Delete" {
				a.mu.Lock()
				ok := a.Root.Remove(id)
				var saveErr error
				if ok {
					saveErr = a.Saver.Save(a.Root)
				}
				a.mu.Unlock()
				a.rebuildTree("")
				if saveErr != nil {
					a.setStatus("[red]save failed:[white] " + saveErr.Error())
				}
			}
		})
	a.Pages.AddPage("confirm", modal, true, true)
	a.App.SetFocus(modal)
}

// magicSelected breaks the highlighted task down via the LLM on a
// goroutine, showing progress and applying the result on the UI thread.
func (a *App) magicSelected() {
	t := a.selected()
	if t == nil {
		return
	}
	id, title := t.ID, t.Title
	a.setStatus(fmt.Sprintf("[yellow]breaking down %q… this can take a few seconds[white]", title))
	go func() {
		scratch := todo.New(title)
		err := a.Engine.Break(scratch, 0)
		a.ui(func() {
			if err != nil {
				a.setStatus("[red]magic failed:[white] " + err.Error())
				return
			}
			a.mu.Lock()
			target := a.Root.Find(id)
			if target != nil {
				target.SetChildren(scratch.Children)
				saveErr := a.Saver.Save(a.Root)
				if saveErr != nil {
					err = saveErr
				}
			}
			a.mu.Unlock()
			if err != nil {
				a.setStatus("[red]save failed:[white] " + err.Error())
				return
			}
			if target == nil {
				a.setStatus("[red]task vanished before magic finished[white]")
				return
			}
			a.rebuildTree(id)
			if n := findNode(a.Tree.GetRoot(), id); n != nil {
				n.SetExpanded(true)
			}
			a.setStatus(fmt.Sprintf("[green]broke %q into %d steps[white]", title, len(scratch.Children)))
		})
	}()
}

// prompt shows a centered single-line input. onDone fires with the trimmed
// text on Enter (empty text is ignored); Esc cancels.
func (a *App) prompt(title, initial string, onDone func(string)) {
	input := tview.NewInputField().SetText(initial)
	input.SetFieldBackgroundColor(tcell.ColorBlack)
	input.SetBorder(true).SetTitle(" " + title + "  [darkgray](Enter to save · Esc to cancel)[white] ").SetTitleAlign(tview.AlignLeft)
	input.SetDoneFunc(func(key tcell.Key) {
		switch key {
		case tcell.KeyEnter:
			text := strings.TrimSpace(input.GetText())
			a.Pages.RemovePage("prompt")
			a.App.SetFocus(a.Tree)
			if text != "" {
				onDone(text)
			}
		case tcell.KeyEscape:
			a.Pages.RemovePage("prompt")
			a.App.SetFocus(a.Tree)
		}
	})
	a.Pages.AddPage("prompt", centeredModal(input, 64, 3), true, true)
	a.App.SetFocus(input)
}

func (a *App) reloadFromDisk() {
	_ = a.Saver.Flush() // persist any pending writes before re-reading
	r, err := todo.Load(a.Path)
	if err != nil {
		a.setStatus("[red]reload failed:[white] " + err.Error())
		return
	}
	a.mu.Lock()
	a.Root.ReplaceWith(r)
	count := a.Root.Count()
	a.mu.Unlock()
	a.rebuildTree("")
	a.setStatus(fmt.Sprintf("reloaded %d tasks from disk", count))
}

func (a *App) quit() {
	a.mu.Lock()
	saveErr := a.Saver.Save(a.Root)
	a.mu.Unlock()
	flushErr := a.Saver.Flush() // durable, fsync'd final write
	if saveErr != nil || flushErr != nil {
		if saveErr != nil {
			fmt.Fprintln(os.Stderr, "save error:", saveErr)
		}
		if flushErr != nil {
			fmt.Fprintln(os.Stderr, "save error:", flushErr)
		}
	}
	a.App.Stop()
}

// ----- LM Studio: settings, model picker, URL ----------------------------

// saveConfig persists the current LM Studio base URL and model.
func (a *App) saveConfig() {
	if a.CfgPath == "" || a.LLM == nil {
		return
	}
	baseURL, model := a.LLM.Snapshot()
	_ = config.Save(a.CfgPath, config.Config{BaseURL: baseURL, Model: model})
}

// cmdSettings shows the current LM Studio endpoint, model, and paths.
func (a *App) cmdSettings() {
	if a.LLM == nil {
		a.setStatus("no LM Studio client configured")
		return
	}
	base, model := a.LLM.Snapshot()
	if base == "" {
		base = "(auto-probe " + strings.Join(llm.DefaultBaseURLs, ", ") + ")"
	}
	if model == "" {
		model = "(auto)"
	}
	text := fmt.Sprintf(`[white]settings

  [yellow]LM Studio URL[white]   %s
  [yellow]Active model[white]    %s
  [yellow]Store[white]           %s
  [yellow]Config[white]          %s

change with:
  [yellow]u[white]   set the LM Studio URL
  [yellow]M[white]   pick a model from a list

press [yellow]Esc[white] or [yellow]q[white] to close.`, base, model, a.Path, a.CfgPath)
	a.showModal("settings", " settings ", text)
}

// promptURL asks for a new LM Studio base URL, re-probes, and persists.
func (a *App) promptURL() {
	if a.LLM == nil {
		a.setStatus("no LM Studio client configured")
		return
	}
	baseURL, _ := a.LLM.Snapshot()
	a.prompt("LM Studio URL", baseURL, func(u string) {
		a.LLM.SetBaseURL(u)
		baseURL, _ := a.LLM.Snapshot()
		a.setStatus("checking " + baseURL + " ...")
		go func() {
			models, err := a.LLM.ChatModels()
			if err != nil {
				a.saveConfig()
				a.ui(func() { a.setStatus("[red]url set, but unreachable:[white] " + err.Error()) })
				return
			}
			_, currentModel := a.LLM.Snapshot()
			if !contains(models, currentModel) {
				a.LLM.SetModel("")
				_ = a.LLM.PickModel()
			}
			a.saveConfig()
			a.ui(func() {
				a.refreshHeader()
				baseURL, currentModel := a.LLM.Snapshot()
				a.setStatus(fmt.Sprintf("[green]connected[white] to %s - %d models, using %s", baseURL, len(models), currentModel))
			})
		}()
	})
}

// cmdModel lists models in an interactive picker, or switches directly when
// args[0] is an id or list number.
func (a *App) cmdModel(args []string) {
	if a.LLM == nil {
		a.setStatus("no LM Studio client configured")
		return
	}
	a.setStatus("loading models from LM Studio …")
	go func() {
		models, err := a.LLM.ChatModels()
		if err != nil {
			a.ui(func() { a.setStatus("[red]could not list models:[white] " + err.Error()) })
			return
		}
		if len(models) == 0 {
			a.ui(func() { a.setStatus("LM Studio has no chat models loaded") })
			return
		}
		if len(args) > 0 {
			target := args[0]
			if n, convErr := strconv.Atoi(target); convErr == nil && n >= 1 && n <= len(models) {
				target = models[n-1]
			}
			if !contains(models, target) {
				a.ui(func() { a.setStatus(fmt.Sprintf("no such model %q", target)) })
				return
			}
			a.LLM.SetModel(target)
			a.saveConfig()
			a.ui(func() {
				a.refreshHeader()
				a.setStatus("active model: " + target)
			})
			return
		}
		a.ui(func() { a.showModelPicker(models) })
	}()
}

// showModelPicker opens a selectable, navigable list of models. Must be
// called on the UI goroutine (it is, via a.ui from cmdModel).
func (a *App) showModelPicker(models []string) {
	list := tview.NewList().ShowSecondaryText(false)
	_, currentModel := a.LLM.Snapshot()
	list.SetBorder(true).
		SetTitle(fmt.Sprintf(" select model  (active: %s) ", currentModel)).
		SetTitleAlign(tview.AlignLeft)
	for i, m := range models {
		label := m
		if m == currentModel {
			label = "• " + m
		}
		shortcut := rune('a' + i)
		if i > 25 {
			shortcut = 0
		}
		model := m
		list.AddItem(label, "", shortcut, func() {
			a.LLM.SetModel(model)
			a.saveConfig()
			a.Pages.RemovePage("models")
			a.App.SetFocus(a.Tree)
			a.refreshHeader()
			a.setStatus("active model: " + model)
		})
	}
	list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEsc {
			a.Pages.RemovePage("models")
			a.App.SetFocus(a.Tree)
			return nil
		}
		return ev
	})
	a.Pages.AddPage("models", centeredModal(list, 64, 18), true, true)
	a.App.SetFocus(list)
}

// promptExport asks for a path and writes JSON or Markdown by extension.
func (a *App) promptExport() {
	a.prompt("export to path (.md for Markdown)", "", func(path string) {
		var data []byte
		a.mu.Lock()
		switch export.PickFormat(path) {
		case "markdown":
			data = []byte(export.Markdown(a.Root) + "\n")
		default:
			var jsonErr error
			data, jsonErr = export.JSON(a.Root)
			if jsonErr != nil {
				a.mu.Unlock()
				a.setStatus("[red]export error:[white] " + jsonErr.Error())
				return
			}
		}
		a.mu.Unlock()
		err := os.WriteFile(path, data, 0o644)
		if err != nil {
			a.setStatus("[red]export error:[white] " + err.Error())
			return
		}
		a.setStatus("exported to " + path)
	})
}

// ----- shared modals & helpers -------------------------------------------

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func displayURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		if strings.HasPrefix(addr, ":") {
			return "http://127.0.0.1" + addr
		}
		return "http://" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// centeredModal floats a primitive in the middle of the screen.
func centeredModal(p tview.Primitive, width, height int) tview.Primitive {
	return tview.NewGrid().
		SetColumns(0, width, 0).
		SetRows(0, height, 0).
		AddItem(p, 1, 1, 1, 1, 0, 0, true)
}

// showModal displays a read-only text modal dismissed with Esc or q.
func (a *App) showModal(page, title, text string) {
	tv := tview.NewTextView().SetText(text).SetDynamicColors(true).SetScrollable(true)
	tv.SetBorder(true).SetTitle(title).SetTitleAlign(tview.AlignLeft)
	tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEsc || ev.Rune() == 'q' {
			a.Pages.RemovePage(page)
			a.App.SetFocus(a.Tree)
			return nil
		}
		return ev
	})
	a.Pages.AddPage(page, centeredModal(tv, 72, 16), true, true)
	a.App.SetFocus(tv)
}

// setStatus flashes a transient message in the footer, then restores the
// help line after a few seconds. Call only on the UI goroutine; from a
// background goroutine wrap it with a.ui(...).
func (a *App) setStatus(s string) {
	a.statusGen++
	gen := a.statusGen
	a.Status.SetText(" " + s)
	time.AfterFunc(4*time.Second, func() {
		a.ui(func() {
			// Only revert if no newer message replaced this one.
			if a.statusGen == gen {
				a.Status.SetText(a.helpLine())
			}
		})
	})
}

// ui runs f on tview's event-loop goroutine and redraws. Use it from
// background goroutines to mutate widgets safely. Never call it from the
// UI goroutine itself — that deadlocks; mutate the primitives directly.
func (a *App) ui(f func()) {
	a.App.QueueUpdateDraw(f)
}

func (a *App) showHelp() {
	help := fmt.Sprintf(`[white]magic todo — navigable task breaker

  [yellow]↑ ↓[white] or [yellow]j k[white]     move between tasks
  [yellow]→ ←[white] or [yellow]l h[white]     expand / collapse subtasks
  [yellow]space[white] / [yellow]Enter[white]   toggle a task done
  [yellow]a[white]               add a top-level task
  [yellow]A[white]               add a subtask under the selected task
  [yellow]e[white]               edit the selected task's title
  [yellow]m[white]               magic-break the task into subtasks (LLM)
  [yellow]d[white] / [yellow]Delete[white]      delete the task (and its subtree)
  [yellow]M[white]               choose the LM Studio model
  [yellow]u[white]               set the LM Studio URL
  [yellow]S[white]               settings overview
  [yellow]X[white]               export the list (JSON, or .md for Markdown)
  [yellow]r[white]               reload from disk
  [yellow]q[white]               save and quit

tasks autosave to [darkgray]%s[white]
press [yellow]Esc[white] or [yellow]q[white] to close this help.`, a.Path)
	a.showModal("help", " help ", help)
}

// ----- tree-node search helpers ------------------------------------------

// findNode returns the descendant node whose task id matches, or nil.
func findNode(n *tview.TreeNode, id string) *tview.TreeNode {
	if id == "" || n == nil {
		return nil
	}
	if t, ok := n.GetReference().(*todo.Task); ok && t.ID == id {
		return n
	}
	for _, c := range n.GetChildren() {
		if f := findNode(c, id); f != nil {
			return f
		}
	}
	return nil
}

// firstSelectable returns the first selectable descendant (depth-first).
func firstSelectable(n *tview.TreeNode) *tview.TreeNode {
	for _, c := range n.GetChildren() {
		if c.GetReference() != nil {
			return c
		}
		if f := firstSelectable(c); f != nil {
			return f
		}
	}
	return nil
}
