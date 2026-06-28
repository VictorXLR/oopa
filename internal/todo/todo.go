// Package todo holds the recursive task tree model and JSON persistence.
package todo

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Task is a single item in the todo tree. Subtasks form a recursive
// breakdown of their parent.
type Task struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	Done     bool    `json:"done"`
	Children []*Task `json:"children,omitempty"`
	Parent   string  `json:"-"` // runtime link, not serialized
}

// New creates a top-level task with a stable-ish ID.
func New(title string) *Task {
	return &Task{ID: newID(), Title: title}
}

// Root is the user's todo list; a list of top-level tasks with metadata.
type Root struct {
	Tasks []*Task `json:"tasks"`
}

// Add appends a top-level task and refreshes its runtime parent links.
func (r *Root) Add(t *Task) {
	if t == nil {
		return
	}
	t.assignParents("")
	r.Tasks = append(r.Tasks, t)
}

// ReplaceWith swaps this root's contents in place, preserving the Root
// pointer shared by the TUI and web server.
func (r *Root) ReplaceWith(other *Root) {
	if other == nil {
		r.Tasks = nil
		return
	}
	r.Tasks = other.Tasks
	r.assignParents("")
}

// AddChild appends child under t and refreshes runtime parent links.
func (t *Task) AddChild(child *Task) {
	if child == nil {
		return
	}
	child.assignParents(t.ID)
	t.Children = append(t.Children, child)
}

// SetChildren replaces t's children and refreshes runtime parent links.
func (t *Task) SetChildren(children []*Task) {
	t.Children = children
	for _, c := range t.Children {
		c.assignParents(t.ID)
	}
}

// Load reads the JSON store from path, returning an empty Root if missing.
func Load(path string) (*Root, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Root{}, nil
	}
	if err != nil {
		return nil, err
	}
	var r Root
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	r.assignParents("")
	return &r, nil
}

// Save writes the store as pretty JSON to path, creating dirs as needed.
// The write is atomic: data goes to a temp file in the same directory and
// is renamed into place, so a crash mid-write can't corrupt the store.
// It does NOT fsync, so it stays off the disk-flush critical path and is
// fast enough to call on every mutation; for a durable flush (e.g. on
// quit) use a Saver.Flush.
func Save(path string, r *Root) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, b, false)
}

// writeAtomic writes b to path via a temp file + rename. When sync is
// true it fsyncs the temp file before renaming for durability.
func writeAtomic(path string, b []byte, sync bool) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".oopa-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if sync {
		if err := tmp.Sync(); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Saver coalesces frequent writes to one file. Mutations call Save, which
// marshals the current tree immediately (cheap, and consistent because the
// caller holds its own lock) but defers the slow disk write to a debounced
// background timer. This keeps disk I/O off the interaction path while
// still persisting every change. Call Flush (e.g. on quit) to force a
// final, durable (fsync'd) write.
type Saver struct {
	path  string
	delay time.Duration

	mu      sync.Mutex
	pending []byte // latest snapshot not yet on disk
	timer   *time.Timer
	err     error
}

// NewSaver builds a debounced saver for path.
func NewSaver(path string) *Saver {
	return &Saver{path: path, delay: 400 * time.Millisecond}
}

// Save records a new snapshot to be written soon. Call while holding the
// lock that guards r so the marshal captures a consistent tree.
func (s *Saver) Save(r *Root) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	s.mu.Lock()
	prevErr := s.err
	s.err = nil
	s.pending = b
	if s.timer == nil {
		s.timer = time.AfterFunc(s.delay, s.writePending)
	}
	s.mu.Unlock()
	return prevErr
}

// writePending writes the latest snapshot (debounced, non-durable).
func (s *Saver) writePending() {
	s.mu.Lock()
	b := s.pending
	s.pending = nil
	s.timer = nil
	s.mu.Unlock()
	if b != nil {
		err := writeAtomic(s.path, b, false)
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
	}
}

// Flush writes any pending snapshot durably (with fsync) and waits for it.
func (s *Saver) Flush() error {
	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	b := s.pending
	prevErr := s.err
	s.err = nil
	s.pending = nil
	s.mu.Unlock()
	if b == nil {
		return prevErr
	}
	if err := writeAtomic(s.path, b, true); err != nil {
		return err
	}
	return prevErr
}

// assignParents walks the tree setting the Parent field to each node's
// parent ID. The extra param is the top-level sentinel ("-").
func (r *Root) assignParents(parentID string) {
	for _, t := range r.Tasks {
		t.assignParents(parentID)
	}
}

func (t *Task) assignParents(parentID string) {
	t.Parent = parentID
	for _, c := range t.Children {
		c.assignParents(t.ID)
	}
}

// Find returns the task with the given id anywhere in the tree.
func (r *Root) Find(id string) *Task {
	for _, t := range r.Tasks {
		if found := t.find(id); found != nil {
			return found
		}
	}
	return nil
}

func (t *Task) find(id string) *Task {
	if t.ID == id {
		return t
	}
	for _, c := range t.Children {
		if f := c.find(id); f != nil {
			return f
		}
	}
	return nil
}

// Remove deletes the task with id (and its subtree) from the tree.
func (r *Root) Remove(id string) bool {
	for i, t := range r.Tasks {
		if t.ID == id {
			r.Tasks = append(r.Tasks[:i], r.Tasks[i+1:]...)
			return true
		}
		if t.remove(id) {
			return true
		}
	}
	return false
}

func (t *Task) remove(id string) bool {
	for i, c := range t.Children {
		if c.ID == id {
			t.Children = append(t.Children[:i], t.Children[i+1:]...)
			return true
		}
		if c.remove(id) {
			return true
		}
	}
	return false
}

// Count returns total tasks including nested ones.
func (r *Root) Count() int {
	n := 0
	for _, t := range r.Tasks {
		n += 1 + countSub(t)
	}
	return n
}

func countSub(t *Task) int {
	n := len(t.Children)
	for _, c := range t.Children {
		n += countSub(c)
	}
	return n
}

// newID returns a random short id, unique across restarts.
func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "t" + hex.EncodeToString(b)
}
