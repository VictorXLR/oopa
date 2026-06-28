package todo

import (
	"path/filepath"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "store.json")

	r := &Root{}
	r.Tasks = append(r.Tasks, New("Move apartment"))
	parent := r.Tasks[0]
	parent.Children = append(parent.Children, New("Email landlord"))
	parent.Children[0].Done = true

	if err := Save(p, r); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Count() != 2 {
		t.Fatalf("count = %d, want 2", loaded.Count())
	}
	if loaded.Tasks[0].Children[0].Done != true {
		t.Fatal("done flag lost in round trip")
	}
	if loaded.Tasks[0].Children[0].Parent != loaded.Tasks[0].ID {
		t.Fatal("parent link not set on load")
	}
}

func TestFindRemove(t *testing.T) {
	r := &Root{}
	top := New("Top")
	top.AddChild(New("Child"))
	r.Add(top)

	childID := top.Children[0].ID
	if r.Find(childID) == nil {
		t.Fatal("could not find child by id")
	}
	if !r.Remove(childID) {
		t.Fatal("could not remove child")
	}
	if r.Find(childID) != nil {
		t.Fatal("child still present after remove")
	}
	if !r.Remove(top.ID) {
		t.Fatal("could not remove top")
	}
	if r.Count() != 0 {
		t.Fatal("expected empty")
	}
}

func TestParentLinksOnMutations(t *testing.T) {
	r := &Root{}
	top := New("Top")
	child := New("Child")
	grandchild := New("Grandchild")
	child.AddChild(grandchild)
	top.SetChildren([]*Task{child})
	r.Add(top)

	if top.Parent != "" {
		t.Fatalf("top parent = %q, want empty", top.Parent)
	}
	if child.Parent != top.ID {
		t.Fatalf("child parent = %q, want %q", child.Parent, top.ID)
	}
	if grandchild.Parent != child.ID {
		t.Fatalf("grandchild parent = %q, want %q", grandchild.Parent, child.ID)
	}
}

func TestReplaceWithPreservesRootPointer(t *testing.T) {
	r := &Root{}
	old := r
	other := &Root{}
	top := New("Loaded")
	top.AddChild(New("Loaded child"))
	other.Add(top)

	r.ReplaceWith(other)

	if r != old {
		t.Fatal("root pointer changed")
	}
	if r.Count() != 2 {
		t.Fatalf("count = %d, want 2", r.Count())
	}
	if r.Tasks[0].Children[0].Parent != r.Tasks[0].ID {
		t.Fatal("parent links not refreshed")
	}
}
