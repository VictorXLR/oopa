// Package export formats a todo tree for export: as JSON (the raw store)
// or as a Markdown checkbox tree, the goblin.tools-friendly way to share
// a list with another human.
package export

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/loopy/oopa/internal/todo"
)

// Format JSON returns the store as 2-space pretty JSON.
func JSON(root *todo.Root) ([]byte, error) {
	return json.MarshalIndent(root, "", "  ")
}

// Markdown renders the tree as a nested Markdown checklist:
//
//   - [x] Move apartment
//   - [ ] Email landlord
//   - [ ] Book movers
//   - [ ] Update address
func Markdown(root *todo.Root) string {
	var b bytes.Buffer
	if len(root.Tasks) == 0 {
		return "_(no tasks)_\n"
	}
	for _, t := range root.Tasks {
		writeMark(&b, t, 0)
	}
	return b.String()
}

func writeMark(b *bytes.Buffer, t *todo.Task, depth int) {
	box := "[ ]"
	if t.Done {
		box = "[x]"
	}
	indent := strings.Repeat("  ", depth)
	fmt.Fprintf(b, "%s- %s %s\n", indent, box, t.Title)
	for _, c := range t.Children {
		writeMark(b, c, depth+1)
	}
}

// PickFormat selects JSON or Markdown based on file extension. Default
// (no extension, unknown) is JSON.
func PickFormat(path string) string {
	if strings.HasSuffix(path, ".md") || strings.HasSuffix(path, ".markdown") {
		return "markdown"
	}
	return "json"
}
