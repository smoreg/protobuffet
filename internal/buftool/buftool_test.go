package buftool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBuf installs a script named buf on PATH. The registry drives the real
// buf binary through exec, so the wrapper is tested the same way: by what a
// binary on PATH does.
func fakeBuf(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "buf")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

func TestFind(t *testing.T) {
	path := fakeBuf(t, "exit 0\n")
	b, err := Find("")
	if err != nil || b.Path() == "" {
		t.Fatalf("Find on PATH: %v", err)
	}
	if b, err := Find(path); err != nil || b.Path() != path {
		t.Errorf("explicit path: %v", err)
	}
	if _, err := Find("/nonexistent/buf"); err == nil {
		t.Error("missing explicit binary accepted")
	}
}

func TestFindReportsMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := Find(""); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestBreakingVerdicts(t *testing.T) {
	// buf exits 100 when it found breaking changes and 1 on a real failure.
	// Treating the two the same would either publish breaking changes or
	// reject every push when the tool is broken.
	t.Run("compatible", func(t *testing.T) {
		fakeBuf(t, "exit 0\n")
		b, _ := Find("")
		res, err := b.Breaking(context.Background(), t.TempDir(), "new.zip", "old.zip")
		if err != nil || !res.Compatible {
			t.Fatalf("res = %+v, err = %v", res, err)
		}
	})
	t.Run("breaking", func(t *testing.T) {
		fakeBuf(t, `echo "catalog.proto:1:1:Field \"2\" was deleted."; exit 100`+"\n")
		b, _ := Find("")
		res, err := b.Breaking(context.Background(), t.TempDir(), "new.zip", "old.zip")
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if res.Compatible {
			t.Error("breaking change reported as compatible")
		}
		if !strings.Contains(res.Output, "was deleted") {
			t.Errorf("findings lost: %q", res.Output)
		}
	})
	t.Run("tool failure", func(t *testing.T) {
		fakeBuf(t, `echo "Failure: cannot read input" >&2; exit 1`+"\n")
		b, _ := Find("")
		if _, err := b.Breaking(context.Background(), t.TempDir(), "new.zip", "old.zip"); err == nil {
			t.Error("a tool failure was reported as a verdict")
		}
	})
}

func TestBreakingPassesInput(t *testing.T) {
	// The input argument must reach buf: without it the check runs against
	// the working directory and silently compares the wrong thing.
	fakeBuf(t, `echo "$@" > "$TMPARGS"; exit 0`+"\n")
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("TMPARGS", argsFile)

	b, _ := Find("")
	if _, err := b.Breaking(context.Background(), t.TempDir(), "new.zip", "old.zip"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if want := "breaking new.zip --against old.zip"; strings.TrimSpace(string(got)) != want {
		t.Errorf("args = %q, want %q", strings.TrimSpace(string(got)), want)
	}
}

func TestLintGenerateVersion(t *testing.T) {
	fakeBuf(t, `case "$1" in
  lint) exit 0 ;;
  generate) echo "$@" > "$TMPARGS"; exit 0 ;;
  --version) echo "1.72.0" ;;
  dep) exit 0 ;;
esac
`)
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("TMPARGS", argsFile)
	b, _ := Find("")
	ctx := context.Background()

	if err := b.Lint(ctx, t.TempDir()); err != nil {
		t.Errorf("Lint: %v", err)
	}
	if err := b.DepUpdate(ctx, t.TempDir()); err != nil {
		t.Errorf("DepUpdate: %v", err)
	}
	if v, err := b.Version(ctx); err != nil || !strings.Contains(v, "1.72.0") {
		t.Errorf("Version = %q, %v", v, err)
	}
	if err := b.Generate(ctx, t.TempDir(), "buf.gen.go.yaml"); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got, _ := os.ReadFile(argsFile)
	if want := "generate --template buf.gen.go.yaml"; strings.TrimSpace(string(got)) != want {
		t.Errorf("generate args = %q, want %q", strings.TrimSpace(string(got)), want)
	}
}

func TestMissingProtocGetsAnActionableError(t *testing.T) {
	// buf reports a missing protoc as a plugin failure, which reads like a
	// broken template rather than a missing tool.
	fakeBuf(t, `echo 'Failure: plugin php: exec: "protoc": executable file not found in $PATH' >&2; exit 1`+"\n")
	b, _ := Find("")
	err := b.Generate(context.Background(), t.TempDir(), "buf.gen.php.yaml")
	if err == nil || !strings.Contains(err.Error(), "protoc is not installed") {
		t.Errorf("err = %v, want the protoc hint", err)
	}
}
