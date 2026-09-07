package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// auditEvent is one line in the audit log. The log answers "who published
// this, from where, and did the gate pass" months after the fact, which
// neither the request log nor the index can.
type auditEvent struct {
	Time      time.Time `json:"time"`
	RequestID string    `json:"request_id,omitempty"`
	Subject   string    `json:"subject"`
	Action    string    `json:"action"`
	Module    string    `json:"module"`
	Version   string    `json:"version,omitempty"`
	Label     string    `json:"label,omitempty"`
	Result    string    `json:"result"`
	Remote    string    `json:"remote,omitempty"`
	Detail    string    `json:"detail,omitempty"`
}

// auditLog appends JSON lines to a file. Writes are serialised because the
// value of the file is that its lines are whole.
type auditLog struct {
	mu   sync.Mutex
	path string
}

func newAuditLog(dataDir string) *auditLog {
	return &auditLog{path: filepath.Join(dataDir, "audit.log")}
}

func (a *auditLog) record(e auditEvent) error {
	if a == nil {
		return nil
	}
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // the write below is what matters
	if _, err := f.Write(append(body, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// tail returns the last n events, newest first.
func (a *auditLog) tail(n int) ([]auditEvent, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	body, err := os.ReadFile(a.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []auditEvent
	for line := range splitLines(body) {
		var e auditEvent
		if err := json.Unmarshal(line, &e); err != nil {
			continue // a truncated line must not hide the rest
		}
		out = append(out, e)
	}
	// Newest first, capped.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out, nil
}

func splitLines(body []byte) func(func([]byte) bool) {
	return func(yield func([]byte) bool) {
		start := 0
		for i, b := range body {
			if b != '\n' {
				continue
			}
			if i > start && !yield(body[start:i]) {
				return
			}
			start = i + 1
		}
		if start < len(body) {
			yield(body[start:])
		}
	}
}
