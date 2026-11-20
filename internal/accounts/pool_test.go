package accounts

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPoolAcquireAndCooldownFailover(t *testing.T) {
	dir := t.TempDir()
	p, err := NewPool(dir)
	if err != nil {
		t.Fatalf("NewPool failed: %v", err)
	}
	a1, err := p.Ensure("acc1")
	if err != nil {
		t.Fatalf("Ensure acc1: %v", err)
	}
	a2, err := p.Ensure("acc2")
	if err != nil {
		t.Fatalf("Ensure acc2: %v", err)
	}
	_ = os.WriteFile(filepath.Join(a1.ConfigDir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"tok1"}}`), 0o600)
	_ = os.WriteFile(filepath.Join(a2.ConfigDir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"tok2"}}`), 0o600)

	picked1, err := p.Acquire(nil)
	if err != nil {
		t.Fatalf("Acquire 1 failed: %v", err)
	}
	if picked1.ID != "acc1" {
		t.Fatalf("expected acc1, got %s", picked1.ID)
	}
	p.Release(picked1.ID)

	// Mark acc1 rate limited -> should failover to acc2
	p.MarkRateLimited("acc1", 5*time.Minute, "rate limit")
	picked2, err := p.Acquire(nil)
	if err != nil {
		t.Fatalf("Acquire 2 failed: %v", err)
	}
	if picked2.ID != "acc2" {
		t.Fatalf("expected failover to acc2, got %s", picked2.ID)
	}
	p.Release(picked2.ID)
}
