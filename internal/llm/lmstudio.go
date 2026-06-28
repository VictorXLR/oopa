// Package llm talks to LM Studio's OpenAI-compatible local server.
//
// LM Studio exposes a chat/completions endpoint. The default port has
// varied across versions (1234, then 11434); rather than guess, we probe
// a short list of candidate hosts and pick the first that responds to
// /v1/models. We use 127.0.0.1 explicitly because LM Studio binds IPv4
// only and "localhost" can resolve to IPv6 [::1] on macOS, breaking
// the connection even on the right port.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURLs is the list of base URLs probed when neither the caller
// nor the LMSTUDIO_URL env var specifies one. First reachable wins.
var DefaultBaseURLs = []string{
	"http://127.0.0.1:11434/v1",
	"http://127.0.0.1:1234/v1",
	"http://127.0.0.1:8080/v1",
}

// Per-request timeouts. We apply these via context deadlines rather than
// mutating the shared http.Client, so concurrent callers (e.g. the web
// server running a magic completion while the settings panel lists
// models) never race on a single Timeout field.
const (
	completionTimeout = 180 * time.Second
	modelsTimeout     = 10 * time.Second
	probeTimeout      = 3 * time.Second
	// maxCompletionTokens caps generation. A breakdown is just a short
	// JSON array, so a tight cap keeps responses fast and bounds the
	// worst case if a model starts rambling.
	maxCompletionTokens = 400
)

// Client wraps an endpoint URL and an optional model name.
type Client struct {
	mu sync.RWMutex

	BaseURL string
	Model   string
	HTTP    *http.Client
	APIKey  string
}

// New builds a client. If baseURL is empty it is left empty and resolved
// lazily on the first call via [Client.PickModel] / [Client.Models].
func New(baseURL, model, apiKey string) *Client {
	c := &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Model:   model,
		// No global Timeout: deadlines are set per request via context so
		// the shared client is never mutated and is safe for concurrent use.
		HTTP:   &http.Client{},
		APIKey: apiKey,
	}
	return c
}

// SetBaseURL points the client at a specific LM Studio endpoint. Passing
// an empty string resets it to auto-probe the defaults on the next call.
func (c *Client) SetBaseURL(u string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.BaseURL = strings.TrimRight(strings.TrimSpace(u), "/")
}

// SetModel sets the model id used for completions.
func (c *Client) SetModel(m string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Model = strings.TrimSpace(m)
}

// Snapshot returns the current endpoint and model safely.
func (c *Client) Snapshot() (baseURL, model string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.BaseURL, c.Model
}

// Clone returns an independent client with the same API key. It is useful
// for non-persisting probes so settings checks do not mutate the active
// client.
func (c *Client) Clone(baseURL, model string) *Client {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return New(baseURL, model, c.APIKey)
}

// isEmbeddingModel reports whether an id looks like an embedding model,
// which cannot serve chat completions and must be skipped when auto-picking.
func isEmbeddingModel(id string) bool {
	l := strings.ToLower(id)
	return strings.Contains(l, "embed") || strings.Contains(l, "reranker") || strings.Contains(l, "rerank")
}

// Message is a single chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// completionRequest is the subset of OpenAI's schema we need. The
// ChatTemplateKwargs field turns off "thinking" mode on Qwen3-style
// reasoning models served by LM Studio — without it they burn all their
// tokens on internal reasoning and never produce an answer.
type completionRequest struct {
	Model              string         `json:"model,omitempty"`
	Messages           []Message      `json:"messages"`
	Temperature        float64        `json:"temperature"`
	MaxTokens          int            `json:"max_tokens,omitempty"`
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
}

// completionResponse captures the parts of OpenAI's response we need.
// Some local servers (LM Studio with reasoning models like Qwen3) put
// the answer into `reasoning_content` instead of `content`; we fall back
// to that when `content` is empty so magic todo keeps working.
type completionResponse struct {
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
	} `json:"choices"`
}

// Complete sends messages and returns the assistant content. If the
// BaseURL is empty it auto-probes the default endpoints first.
func (c *Client) Complete(messages []Message) (string, error) {
	if err := c.ensureBaseURL(); err != nil {
		return "", err
	}
	baseURL, model, apiKey, httpClient := c.requestState()
	// Reasoning models like Qwen3 can spend all their tokens "thinking"
	// and never emit a final answer. Give a generous cap and ask the
	// server to give us a real answer.
	reqBody := completionRequest{
		Model:              model,
		Messages:           messages,
		Temperature:        0.4,
		MaxTokens:          maxCompletionTokens,
		ChatTemplateKwargs: map[string]any{"enable_thinking": false},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}
	url := baseURL + "/chat/completions"
	ctx, cancel := context.WithTimeout(context.Background(), completionTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm: connect to %s: %w", url, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("llm: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var out completionResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("llm: bad response: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", errors.New("llm: empty completion")
	}
	msg := out.Choices[0].Message
	// Prefer content; fall back to reasoning_content which reasoning
	// models (Qwen3, DeepSeek-R1) populate while leaving content empty.
	if strings.TrimSpace(msg.Content) != "" {
		return msg.Content, nil
	}
	if strings.TrimSpace(msg.ReasoningContent) != "" {
		return msg.ReasoningContent, nil
	}
	return "", errors.New("llm: model returned empty content (it may have spent all tokens on reasoning — try a larger model or raise max_tokens)")
}

// Models returns the list of loaded model ids from /v1/models.
func (c *Client) Models() ([]string, error) {
	if err := c.ensureBaseURL(); err != nil {
		return nil, err
	}
	ids, _, err := c.modelsAt()
	return ids, err
}

// modelsAt returns the model list and the base URL used for the request.
func (c *Client) modelsAt() ([]string, string, error) {
	if err := c.ensureBaseURL(); err != nil {
		return nil, "", err
	}
	baseURL, _, apiKey, httpClient := c.requestState()
	ctx, cancel := context.WithTimeout(context.Background(), modelsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return nil, baseURL, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, baseURL, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, baseURL, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, baseURL, err
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		ids = append(ids, m.ID)
	}
	return ids, baseURL, nil
}

// ensureBaseURL resolves c.BaseURL on the first call. If the user gave
// us a URL (or set LMSTUDIO_URL) we honor it as-is. Otherwise we probe
// the default hosts and pick the one that answers /v1/models. Stays
// resolved for the rest of the process.
func (c *Client) ensureBaseURL() (retErr error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.HTTP == nil {
		c.HTTP = &http.Client{}
	}
	if c.BaseURL != "" {
		return nil
	}
	defer func() {
		if retErr != nil {
			retErr = fmt.Errorf("llm: could not reach LM Studio on %v (set LMSTUDIO_URL): %w", DefaultBaseURLs, retErr)
		}
	}()
	var firstErr error
	for _, base := range DefaultBaseURLs {
		// Try a quick /models probe with a short per-request deadline.
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
		if c.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.APIKey)
		}
		resp, err := c.HTTP.Do(req)
		cancel()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		// Drain and close to reuse the connection.
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			if firstErr == nil {
				firstErr = fmt.Errorf("HTTP %d at %s", resp.StatusCode, base)
			}
			continue
		}
		c.BaseURL = base
		return nil
	}
	if firstErr != nil {
		return firstErr
	}
	return errors.New("llm: no LM Studio server found")
}

func (c *Client) requestState() (baseURL, model, apiKey string, httpClient *http.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.HTTP == nil {
		c.HTTP = &http.Client{}
	}
	return c.BaseURL, c.Model, c.APIKey, c.HTTP
}

// PickModel auto-selects an available model when none is set, preferring
// fast non-reasoning instruct models (gemma, llama-instruct, qwen2.5-
// instruct, mistral) over reasoning models (qwen3, deepseek-r1, o1) which
// burn tokens on "thinking", and skipping embedding models entirely. Also
// resolves the BaseURL by probing defaults; call once at startup.
func (c *Client) PickModel() error {
	ids, _, err := c.modelsAt()
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return errors.New("llm: LM Studio has no models loaded")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Model == "" {
		c.Model = pickBestModel(ids)
	}
	return nil
}

// WarmUp sends a tiny throwaway completion so LM Studio loads the selected
// model into memory now, rather than on the user's first real request — a
// cold model load can take many seconds. Best-effort; errors are ignored.
func (c *Client) WarmUp() {
	_, _ = c.Complete([]Message{{Role: "user", Content: "Reply with the single word: ready"}})
}

// ChatModels returns the loaded models that can serve chat completions
// (i.e. excluding embedding/reranker models).
func (c *Client) ChatModels() ([]string, error) {
	ids, err := c.Models()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !isEmbeddingModel(id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// pickBestModel ranks model ids and returns the best one for our use:
// chat completion of small JSON tasks, fast, non-reasoning.
func pickBestModel(ids []string) string {
	score := func(id string) int {
		s := strings.ToLower(id)
		// Hard avoid: reasoning models / embedding models.
		switch {
		case strings.Contains(s, "embed"):
			return -100
		case strings.Contains(s, "qwen3"), strings.Contains(s, "deepseek-r1"), strings.Contains(s, "-r1"), strings.Contains(s, "o1"), strings.Contains(s, "-reasoning"):
			return -10
		}
		// Prefer known-good instruct lineages.
		prefer := []string{"gemma", "llama", "mistral", "phi", "qwen2.5", "qwen2-", "yi"}
		score := 0
		for i, p := range prefer {
			if strings.Contains(s, p) {
				score = 100 - i // earlier = higher
				break
			}
		}
		// "instruct" or "it" suffix is a plus.
		if strings.Contains(s, "instruct") || strings.HasSuffix(s, "-it") {
			score += 5
		}
		// Smaller is faster and good enough for tiny JSON tasks.
		if strings.Contains(s, "0.5b") || strings.Contains(s, "1.5b") || strings.Contains(s, "3b") || strings.Contains(s, "7b") || strings.Contains(s, "8b") {
			score += 2
		}
		if score == 0 {
			score = 1 // neutral default
		}
		return score
	}
	best := ids[0]
	bestScore := score(best)
	for _, id := range ids[1:] {
		if sc := score(id); sc > bestScore {
			best, bestScore = id, sc
		}
	}
	return best
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
