package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// The CLI posts only with an explicit -post: run without it must not submit
// anything, whatever else it is given.
func TestRunWithoutPostFlagSubmitsNothing(t *testing.T) {
	var submits, decisions atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			submits.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		io.WriteString(w, `{"error":"E_NOT_FOUND","detail":"no such channel"}`)
	}))
	defer hub.Close()
	or := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decisions.Add(1)
		w.WriteHeader(500)
	}))
	defer or.Close()
	dir := t.TempDir()
	cfg := map[string]any{
		"hub_url": hub.URL, "openrouter_url": or.URL, "sources": []string{"llm-papers"},
		"topics": []map[string]any{{"slug": "top-llm-papers", "title": "T", "describe": "d", "from": []string{"llm-papers"},
			"about": "A model selects the items; run by the operator."}},
	}
	raw, _ := json.Marshal(cfg)
	cfgPath := filepath.Join(dir, "c.json")
	os.WriteFile(cfgPath, raw, 0o600)
	keyPath := filepath.Join(dir, "editor.key")
	if code := run([]string{"genkey", keyPath}); code != 0 {
		t.Fatalf("genkey exit %d", code)
	}
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	state := filepath.Join(dir, "state")
	// The source channel is missing, so the run fails (exit 1) before any
	// model call; what matters is that nothing was submitted and no state
	// was written.
	code := run([]string{"run", "-config", cfgPath, "-key", keyPath, "-state", state})
	if submits.Load() != 0 {
		t.Fatalf("run without -post submitted %d time(s)", submits.Load())
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("run without -post created the state dir: %v", err)
	}
	if code != 1 {
		t.Fatalf("exit %d, want 1 (missing source channel)", code)
	}
}

func TestKeyIsNeverAFlag(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`"openrouter-key"`, `"api-key"`, `"key-value"`} {
		if strings.Contains(string(src), bad) {
			t.Fatalf("main.go defines a flag that takes the key itself: %s", bad)
		}
	}
}
