package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"mini-cage-relay/internal/accounts"
	"mini-cage-relay/internal/claude"
	"mini-cage-relay/internal/exitpolicy"
	"mini-cage-relay/internal/safety"
)

func TestOpenAIBlockedWhenSafetyFails(t *testing.T) {
	dir := t.TempDir()
	pool, _ := accounts.NewPool(dir)
	checker := safety.NewChecker(safety.CheckerConfig{AccountsDir: dir}, exitpolicy.DefaultPolicy())
	runner := claude.NewRunner("claude", "sonnet", nil)
	srv := NewServer(ServerConfig{APIKey: "test-key"}, checker, pool, runner)

	body := bytes.NewBufferString(`{"model":"sonnet","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when safety checks have not passed, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestUnauthorizedRejected(t *testing.T) {
	dir := t.TempDir()
	pool, _ := accounts.NewPool(dir)
	checker := safety.NewChecker(safety.CheckerConfig{AccountsDir: dir}, exitpolicy.DefaultPolicy())
	runner := claude.NewRunner("claude", "sonnet", nil)
	srv := NewServer(ServerConfig{APIKey: "secret-key"}, checker, pool, runner)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without API key, got %d", rec.Code)
	}

	// With valid key -> 200
	req2 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req2.Header.Set("Authorization", "Bearer secret-key")
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid API key, got %d", rec2.Code)
	}
	var parsed struct {
		Object string `json:"object"`
	}
	_ = json.Unmarshal(rec2.Body.Bytes(), &parsed)
	if parsed.Object != "list" {
		t.Fatalf("unexpected /v1/models body: %s", rec2.Body.String())
	}
	_ = os.WriteFile(filepath.Join(dir, "ok"), []byte("1"), 0o600)
}
