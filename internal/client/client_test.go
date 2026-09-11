package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smoreg/protobuffet/internal/client"
)

// TestRetriesTransientFailures covers a registry restart or a proxy blip in
// the middle of a CI publish: those should cost a retry, not a red build.
func TestRetriesTransientFailures(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, "restarting", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"modules":[{"module":"acme/catalog"}]}`)) //nolint:errcheck
	}))
	defer ts.Close()

	c := client.New(ts.URL, "token")
	mods, err := c.ListModules(context.Background())
	if err != nil {
		t.Fatalf("ListModules: %v", err)
	}
	if len(mods) != 1 || calls.Load() != 3 {
		t.Errorf("modules = %v after %d calls", mods, calls.Load())
	}
}

func TestGivesUpAfterThreeAttempts(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "still down", http.StatusBadGateway)
	}))
	defer ts.Close()

	_, err := client.New(ts.URL, "token").ListModules(context.Background())
	if err == nil {
		t.Fatal("a permanently failing registry looked healthy")
	}
	if !strings.Contains(err.Error(), "after 3 attempts") {
		t.Errorf("err = %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}
}

func TestDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer ts.Close()

	// Retrying a 403 only delays the error the caller has to act on.
	if _, err := client.New(ts.URL, "token").ListModules(context.Background()); err == nil {
		t.Fatal("403 was not reported")
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}

func TestRespectsContextCancellation(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := client.New(ts.URL, "token").ListModules(ctx); err == nil {
		t.Fatal("expected an error")
	}
	// Without honouring the context the backoff would run the full sequence.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %s, ignoring the deadline", elapsed)
	}
}

func TestTimeoutIsConfigurable(t *testing.T) {
	t.Setenv("BUFFET_TIMEOUT", "5s")
	if got := client.New("http://example.invalid", "t").HTTP.Timeout; got != 5*time.Second {
		t.Errorf("timeout = %s, want 5s", got)
	}
	t.Setenv("BUFFET_TIMEOUT", "nonsense")
	if got := client.New("http://example.invalid", "t").HTTP.Timeout; got != 2*time.Minute {
		t.Errorf("bad value should fall back to the default, got %s", got)
	}
}
