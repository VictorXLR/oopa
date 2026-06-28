// Package breakdown turns a single task into smaller subtasks using an
// LLM client. It produces a nested tree by calling itself recursively
// up to MaxDepth levels deep.
package breakdown

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/loopy/oopa/internal/llm"
	"github.com/loopy/oopa/internal/todo"
)

// Engine performs recursive task breakdowns.
type Engine struct {
	LLM         *llm.Client
	MaxDepth    int
	MaxChildren int
}

// New builds an engine with sane defaults. MaxDepth is 1 so a single
// "magic" expands one level on demand (goblin.tools style) rather than
// firing dozens of recursive LLM calls. Raise MaxDepth to opt into
// deeper auto-breakdown.
func New(c *llm.Client) *Engine {
	return &Engine{LLM: c, MaxDepth: 1, MaxChildren: 6}
}

// Break replaces the task's children with LLM-generated subtasks. If
// depth exceeds MaxDepth, the task is left as a leaf (the model gets to
// decide granularity at higher levels and we cap to avoid runaway loops).
func (e *Engine) Break(t *todo.Task, depth int) error {
	if depth >= e.MaxDepth {
		return nil
	}
	children, err := e.ask(t.Title, depth)
	if err != nil {
		return err
	}
	if len(children) == 0 {
		return nil
	}
	if len(children) > e.MaxChildren {
		children = children[:e.MaxChildren]
	}
	t.SetChildren(nil)
	for _, title := range children {
		child := todo.New(title)
		t.AddChild(child)
		// Recurse one level — goblin.tools style — but stop if the model
		// is asking for boiling-the-ocean splits.
		if err := e.Break(child, depth+1); err != nil {
			// Soft fail: keep this subtree's children empty but continue.
			continue
		}
	}
	return nil
}

const systemPrompt = `You are Magic Todo, an assistant that breaks overwhelming
tasks into tiny, concrete, doable steps. The user gives you a single task;
you reply ONLY with a JSON array of strings, each string being one subtask.
- 3 to 6 subtasks is ideal; fewer is okay, never more than 8.
- Each subtask must be a small, concrete action the user could actually do.
- Use plain imperative voice ("Email the landlord", not "You should email...").
- Do not include the original task in the output.
- Do not wrap the JSON in markdown, do not add commentary, do not explain.`

// ask asks the LLM to break a single task title into subtask strings.
func (e *Engine) ask(title string, depth int) ([]string, error) {
	if strings.TrimSpace(title) == "" {
		return nil, nil
	}
	userMsg := fmt.Sprintf("Break this task into concrete substeps: %q", title)
	resp, err := e.LLM.Complete([]llm.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userMsg},
	})
	if err != nil {
		return nil, err
	}
	return parseJSONList(resp)
}

// parseJSONList extracts a JSON array of strings from a possibly-noisy
// model response. If the model insists on markdown fences or extra
// prose, we slice out the first balanced chunk.
func parseJSONList(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	s = stripThink(s)
	// Strip ``` fences if present.
	if strings.HasPrefix(s, "```") {
		if idx := strings.Index(s, "\n"); idx >= 0 {
			s = s[idx+1:]
		}
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = s[:i]
		}
		s = strings.TrimSpace(s)
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err == nil {
		return clean(out), nil
	}
	// Fallback: find first '[' and matching ']'.
	start := strings.IndexByte(s, '[')
	end := strings.LastIndexByte(s, ']')
	if start >= 0 && end > start {
		if err := json.Unmarshal([]byte(s[start:end+1]), &out); err == nil {
			return clean(out), nil
		}
	}
	return nil, fmt.Errorf("breakdown: could not parse model output: %q", truncate(s, 200))
}

// stripThink drops any chain-of-thought a model emits before its answer.
// Reasoning models (and some templates) wrap thinking in <think>…</think>
// even when asked not to; we keep only what follows the final closing tag.
func stripThink(s string) string {
	if i := strings.LastIndex(s, "</think>"); i >= 0 {
		return strings.TrimSpace(s[i+len("</think>"):])
	}
	return s
}

func clean(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
