package tui

import (
	"fmt"
	"sync"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/loopy/oopa/internal/todo"
)

func newTreeTestApp() *App {
	root := tview.NewTreeNode("root").SetSelectable(false)
	return &App{
		Tree: tview.NewTreeView().
			SetRoot(root).
			SetCurrentNode(root),
	}
}

func TestInsertTaskNodeReplacesEmptyHint(t *testing.T) {
	a := newTreeTestApp()
	a.Tree.GetRoot().AddChild(emptyTreeHint())
	task := todo.New("first")

	if !a.insertTaskNode("", task) {
		t.Fatal("insertTaskNode returned false")
	}

	children := a.Tree.GetRoot().GetChildren()
	if len(children) != 1 {
		t.Fatalf("root has %d children, want 1", len(children))
	}
	if got := children[0].GetReference(); got != task {
		t.Fatalf("child reference = %v, want task", got)
	}
	if got := a.Tree.GetCurrentNode(); got != children[0] {
		t.Fatalf("current node = %v, want inserted node", got)
	}
}

func TestUpdateTaskNode(t *testing.T) {
	a := newTreeTestApp()
	task := todo.New("old")
	a.Tree.GetRoot().AddChild(a.makeNode(task))

	if !a.updateTaskNode(task.ID, "[x] new", tcell.ColorGray) {
		t.Fatal("updateTaskNode returned false")
	}

	node := a.Tree.GetRoot().GetChildren()[0]
	if got := node.GetText(); got != "[x] new" {
		t.Fatalf("node text = %q, want %q", got, "[x] new")
	}
	if got := node.GetColor(); got != tcell.ColorGray {
		t.Fatalf("node color = %v, want %v", got, tcell.ColorGray)
	}
}

func TestRemoveTaskNodeRestoresEmptyHint(t *testing.T) {
	a := newTreeTestApp()
	task := todo.New("remove me")
	a.Tree.GetRoot().AddChild(a.makeNode(task))

	if !a.removeTaskNode(task.ID) {
		t.Fatal("removeTaskNode returned false")
	}

	children := a.Tree.GetRoot().GetChildren()
	if len(children) != 1 {
		t.Fatalf("root has %d children, want 1", len(children))
	}
	if got := children[0].GetReference(); got != nil {
		t.Fatalf("empty hint reference = %v, want nil", got)
	}
}

func TestReplaceTaskChildren(t *testing.T) {
	a := newTreeTestApp()
	parent := todo.New("parent")
	child := todo.New("child")
	parentNode := a.makeNode(parent)
	a.Tree.GetRoot().AddChild(parentNode)

	if !a.replaceTaskChildren(parent.ID, []*tview.TreeNode{a.makeNode(child)}) {
		t.Fatal("replaceTaskChildren returned false")
	}

	children := parentNode.GetChildren()
	if len(children) != 1 {
		t.Fatalf("parent node has %d children, want 1", len(children))
	}
	if got := children[0].GetReference(); got != child {
		t.Fatalf("child reference = %v, want child task", got)
	}
	if !parentNode.IsExpanded() {
		t.Fatal("parent node is collapsed, want expanded")
	}
}

func BenchmarkRebuildTreeLarge(b *testing.B) {
	a, _ := newLargeTreeTestApp(250, 8)

	b.ReportAllocs()
	for b.Loop() {
		a.rebuildTree("")
	}
}

func BenchmarkUpdateTaskNodeLarge(b *testing.B) {
	a, targetID := newLargeTreeTestApp(250, 8)

	b.ReportAllocs()
	for b.Loop() {
		if !a.updateTaskNode(targetID, "[x] updated", tcell.ColorGray) {
			b.Fatal("updateTaskNode returned false")
		}
	}
}

func BenchmarkReplaceTaskChildrenLarge(b *testing.B) {
	a, targetID := newLargeTreeTestApp(250, 8)
	children := []*tview.TreeNode{
		a.makeNode(&todo.Task{ID: "replacement-1", Title: "replacement 1"}),
		a.makeNode(&todo.Task{ID: "replacement-2", Title: "replacement 2"}),
		a.makeNode(&todo.Task{ID: "replacement-3", Title: "replacement 3"}),
	}

	b.ReportAllocs()
	for b.Loop() {
		if !a.replaceTaskChildren(targetID, children) {
			b.Fatal("replaceTaskChildren returned false")
		}
	}
}

func newLargeTreeTestApp(topLevel, childrenPerTask int) (*App, string) {
	root := &todo.Root{}
	targetID := ""
	for i := 0; i < topLevel; i++ {
		task := &todo.Task{
			ID:    fmt.Sprintf("task-%03d", i),
			Title: fmt.Sprintf("task %03d", i),
		}
		for j := 0; j < childrenPerTask; j++ {
			child := &todo.Task{
				ID:     fmt.Sprintf("task-%03d-child-%03d", i, j),
				Title:  fmt.Sprintf("child %03d.%03d", i, j),
				Parent: task.ID,
			}
			task.Children = append(task.Children, child)
			targetID = child.ID
		}
		root.Tasks = append(root.Tasks, task)
	}

	a := newTreeTestApp()
	a.Root = root
	a.mu = &sync.Mutex{}
	a.rebuildTree(targetID)
	return a, targetID
}
