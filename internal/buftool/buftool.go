// Package buftool shells out to the buf CLI. protobuffet deliberately does not
// reimplement protobuf compilation, linting or compatibility checking: buf is
// the reference implementation and is Apache-2.0, so the registry drives it.
package buftool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ErrNotFound means the buf binary is not available.
var ErrNotFound = errors.New("buf binary not found in PATH")

// Buf is a located buf binary.
type Buf struct{ path string }

// Find locates buf. An explicit path wins; otherwise PATH is searched.
//
// Note for shell users: oh-my-zsh's brew plugin defines an alias named "buf",
// which shadows the real binary in interactive shells but not here, because
// exec does not expand aliases.
func Find(explicit string) (*Buf, error) {
	if explicit != "" {
		if _, err := exec.LookPath(explicit); err != nil {
			return nil, fmt.Errorf("buf at %q: %w", explicit, err)
		}
		return &Buf{path: explicit}, nil
	}
	p, err := exec.LookPath("buf")
	if err != nil {
		return nil, ErrNotFound
	}
	return &Buf{path: p}, nil
}

// Path reports the binary location.
func (b *Buf) Path() string { return b.path }

func (b *Buf) run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, b.path, args...)
	cmd.Dir = dir
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	combined := strings.TrimSpace(out.String() + "\n" + errBuf.String())
	if err != nil {
		return combined, fmt.Errorf("buf %s: %w\n%s", strings.Join(args, " "), err, combined)
	}
	return combined, nil
}

// Version returns the buf version string.
func (b *Buf) Version(ctx context.Context) (string, error) {
	return b.run(ctx, "", "--version")
}

// Lint runs the module's lint rules.
func (b *Buf) Lint(ctx context.Context, dir string) error {
	_, err := b.run(ctx, dir, "lint")
	return err
}

// Generate runs code generation with a template, writing wherever the
// template says.
func (b *Buf) Generate(ctx context.Context, dir, template string) error {
	args := []string{"generate"}
	if template != "" {
		args = append(args, "--template", template)
	}
	_, err := b.run(ctx, dir, args...)
	return err
}

// DepUpdate resolves module dependencies into buf.lock.
func (b *Buf) DepUpdate(ctx context.Context, dir string) error {
	_, err := b.run(ctx, dir, "dep", "update")
	return err
}

// BreakingResult carries the outcome of a compatibility check.
type BreakingResult struct {
	Compatible bool
	Output     string
}

// Breaking compares an input against a previous revision. Both may be a
// directory, a zip archive or any other input buf accepts; an empty input
// means "the module in dir". A result with Compatible false lists the
// offending changes in Output.
func (b *Buf) Breaking(ctx context.Context, dir, input, against string) (BreakingResult, error) {
	args := []string{"breaking"}
	if input != "" {
		args = append(args, input)
	}
	args = append(args, "--against", against)
	out, err := b.run(ctx, dir, args...)
	if err == nil {
		return BreakingResult{Compatible: true, Output: out}, nil
	}
	// buf exits 100 when it found breaking changes, and 1 on real failures
	// such as an unreadable input. Only the former is a verdict.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 100 {
		return BreakingResult{Compatible: false, Output: out}, nil
	}
	if strings.Contains(out, "Previous version") || strings.Contains(out, "breaking") {
		return BreakingResult{Compatible: false, Output: out}, nil
	}
	return BreakingResult{}, err
}
