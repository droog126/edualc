package main

import (
	"reflect"
	"testing"
)

func TestExtractCommandsIgnoresQuotedPipes(t *testing.T) {
	cmd := `cd /workspace/myapp && ls; grep -n "alpha::\|Beta{\|gamma\|delta" *.rs | grep -v "^alpha" && gh pr list`
	got := extractCommands(cmd)
	want := []string{"gh"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestExtractCommandsHandlesHeredocAndTimeout(t *testing.T) {
	cmd1 := `CARGO_BUILD_JOBS=2 timeout 590 cargo check -p my-core 2>&1 | grep -E "^(crates/(core|api)/|error)|Finished|warning" | head -30`
	got1 := extractCommands(cmd1)
	want1 := []string{"cargo"}
	if !reflect.DeepEqual(got1, want1) {
		t.Fatalf("expected %v, got %v", want1, got1)
	}

	cmd2 := "python3 - <<'EOF'\nimport pathlib\nmod foo\nmy-service bar\nEOF\n"
	got2 := extractCommands(cmd2)
	want2 := []string{"python3"}
	if !reflect.DeepEqual(got2, want2) {
		t.Fatalf("expected %v, got %v", want2, got2)
	}
}
