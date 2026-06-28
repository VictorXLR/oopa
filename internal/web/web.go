// Package web serves an embedded HTML UI plus a small JSON API on top of
// the todo store and breakdown engine. Same engine, same data, reachable
// from a browser — one binary, one source of truth.
package web

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/loopy/oopa/internal/breakdown"
	"github.com/loopy/oopa/internal/config"
	"github.com/loopy/oopa/internal/llm"
	"github.com/loopy/oopa/internal/todo"
)

//go:embed index.html
var indexHTML string

// Server serves the web UI/API on top of a shared todo store. The store
// (Root), its guarding mutex (Mu) and its debounced Saver are injected so
// the same data can be shared with the TUI running in the same process —
// a single source of truth guarded by one lock.
type Server struct {
	Mu      *sync.Mutex
	Root    *todo.Root
	Path    string
	Saver   *todo.Saver
	Engine  *breakdown.Engine
	LLM     *llm.Client
	CfgPath string
}

// New wires a Server. mu and saver may be shared with other surfaces
// (e.g. the TUI); pass fresh ones for a standalone server.
func New(root *todo.Root, path string, engine *breakdown.Engine, client *llm.Client, cfgPath string, mu *sync.Mutex, saver *todo.Saver) *Server {
	if mu == nil {
		mu = &sync.Mutex{}
	}
	if saver == nil {
		saver = todo.NewSaver(path)
	}
	return &Server{Root: root, Path: path, Engine: engine, LLM: client, CfgPath: cfgPath, Mu: mu, Saver: saver}
}

// Listen starts the HTTP server and blocks.
func (s *Server) Listen(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.serveIndex)
	mux.HandleFunc("/api/root", s.getRoot)
	mux.HandleFunc("/api/task", s.postTask)     // POST  -> add top-level, query ?title=
	mux.HandleFunc("/api/task/", s.taskByID)    // DELETE -> remove
	mux.HandleFunc("/api/magic/", s.magic)      // POST -> break down
	mux.HandleFunc("/api/toggle/", s.toggle)    // POST -> toggle done
	mux.HandleFunc("/api/models", s.getModels)  // GET  -> list LM Studio models + current
	mux.HandleFunc("/api/settings", s.settings) // GET/POST -> read/update url + model
	mux.HandleFunc("/api/probe", s.probe)       // POST -> non-persisting settings probe
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
		// Keep a header-read deadline for robustness, but do NOT set a
		// WriteTimeout: a magic call runs the LLM inside the handler before
		// any byte of the response is written, and the LLM completion
		// deadline (see internal/llm) is ~180s. A WriteTimeout below that
		// would cut the response off mid-completion and surface as a
		// connection-reset on the web client only — magic worked from the
		// TUI (which has no such timeout) but failed from the browser.
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
	}
	return srv.ListenAndServe()
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(indexHTML))
}

func (s *Server) getRoot(w http.ResponseWriter, r *http.Request) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	writeJSON(w, s.Root)
}

func (s *Server) postTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	title := strings.TrimSpace(r.URL.Query().Get("title"))
	if title == "" {
		http.Error(w, "missing ?title", http.StatusBadRequest)
		return
	}
	s.Mu.Lock()
	t := todo.New(title)
	s.Root.Add(t)
	err := s.Saver.Save(s.Root)
	s.Mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, t)
}

func (s *Server) taskByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/task/")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	s.Mu.Lock()
	defer s.Mu.Unlock()
	switch r.Method {
	case http.MethodDelete:
		if !s.Root.Remove(id) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err := s.Saver.Save(s.Root); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) magic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/magic/")

	// Snapshot the title under lock, then do the long-running LLM call on a
	// detached scratch task so we never mutate the shared tree while the
	// mutex is released.
	s.Mu.Lock()
	orig := s.Root.Find(id)
	if orig == nil {
		s.Mu.Unlock()
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	scratch := todo.New(orig.Title)
	s.Mu.Unlock()

	if err := s.Engine.Break(scratch, 0); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// Re-find under lock: the task may have been removed meanwhile.
	s.Mu.Lock()
	defer s.Mu.Unlock()
	t := s.Root.Find(id)
	if t == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	t.SetChildren(scratch.Children)
	if err := s.Saver.Save(s.Root); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, t)
}

func (s *Server) toggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/toggle/")
	s.Mu.Lock()
	defer s.Mu.Unlock()
	t := s.Root.Find(id)
	if t == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	t.Done = !t.Done
	if err := s.Saver.Save(s.Root); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, t)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// settingsResponse is the shape returned by /api/settings and /api/models.
type settingsResponse struct {
	BaseURL  string   `json:"base_url"`
	Model    string   `json:"model"`
	Models   []string `json:"models"`
	Defaults []string `json:"defaults"`
	Error    string   `json:"error,omitempty"`
}

// getModels lists the chat models LM Studio currently exposes, plus the
// active model and base URL. GET only.
func (s *Server) getModels(w http.ResponseWriter, r *http.Request) {
	if s.LLM == nil {
		http.Error(w, "no LM Studio client configured", http.StatusServiceUnavailable)
		return
	}
	models, err := s.LLM.ChatModels()
	baseURL, model := s.LLM.Snapshot()
	resp := settingsResponse{
		BaseURL:  baseURL,
		Model:    model,
		Defaults: llm.DefaultBaseURLs,
	}
	if err != nil {
		resp.Error = err.Error()
	} else {
		resp.Models = models
	}
	writeJSON(w, resp)
}

// settings reads (GET) or updates (POST) the LM Studio base URL and model.
// POST body: {"base_url":"...","model":"..."}. Either field may be omitted.
// On a URL change we re-probe; if the active model is no longer offered we
// auto-pick the best one. Changes are persisted to the config file.
func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	if s.LLM == nil {
		http.Error(w, "no LM Studio client configured", http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodPost {
		var in struct {
			BaseURL string `json:"base_url"`
			Model   string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(in.BaseURL) != "" {
			s.LLM.SetBaseURL(in.BaseURL)
		}
		if strings.TrimSpace(in.Model) != "" {
			s.LLM.SetModel(in.Model)
		}
		// Validate the endpoint and reconcile the model.
		models, err := s.LLM.ChatModels()
		_, currentModel := s.LLM.Snapshot()
		if err == nil && in.Model == "" && !containsStr(models, currentModel) {
			s.LLM.SetModel("")
			_ = s.LLM.PickModel()
		}
		baseURL, model := s.LLM.Snapshot()
		if s.CfgPath != "" {
			_ = config.Save(s.CfgPath, config.Config{BaseURL: baseURL, Model: model})
		}
		resp := settingsResponse{
			BaseURL:  baseURL,
			Model:    model,
			Models:   models,
			Defaults: llm.DefaultBaseURLs,
		}
		if err != nil {
			resp.Error = err.Error()
		}
		writeJSON(w, resp)
		return
	}
	s.getModels(w, r)
}

func (s *Server) probe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.LLM == nil {
		http.Error(w, "no LM Studio client configured", http.StatusServiceUnavailable)
		return
	}
	var in struct {
		BaseURL string `json:"base_url"`
		Model   string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	probe := s.LLM.Clone(in.BaseURL, in.Model)
	models, err := probe.ChatModels()
	if err == nil && strings.TrimSpace(in.Model) == "" {
		_ = probe.PickModel()
	}
	baseURL, model := probe.Snapshot()
	resp := settingsResponse{
		BaseURL:  baseURL,
		Model:    model,
		Models:   models,
		Defaults: llm.DefaultBaseURLs,
	}
	if err != nil {
		resp.Error = err.Error()
	}
	writeJSON(w, resp)
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
