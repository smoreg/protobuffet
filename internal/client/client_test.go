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

// registryStub answers the endpoints the client calls, recording what it saw.
type registryStub struct {
	t        *testing.T
	lastPath string
	lastAuth string
	body     string
	status   int
}

func (s *registryStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.lastPath = r.URL.Path + "?" + r.URL.RawQuery
	s.lastAuth = r.Header.Get("Authorization")
	w.Header().Set("Content-Type", "application/json")
	if s.status != 0 {
		w.WriteHeader(s.status)
	}
	w.Write([]byte(s.body)) //nolint:errcheck
}

func TestClientEndpointsAndAuth(t *testing.T) {
	stub := &registryStub{t: t, body: `{}`}
	ts := httptest.NewServer(stub)
	defer ts.Close()
	c := client.New(ts.URL, "secret-token")
	ctx := context.Background()

	t.Run("whoami", func(t *testing.T) {
		stub.body = `{"subject":"ci","scopes":["write"],"modules":["team/*"]}`
		id, err := c.Whoami(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if id.Subject != "ci" || id.Modules[0] != "team/*" {
			t.Errorf("identity = %+v", id)
		}
		// The token travels as a bearer credential on every call.
		if stub.lastAuth != "Bearer secret-token" {
			t.Errorf("authorization = %q", stub.lastAuth)
		}
	})

	t.Run("module", func(t *testing.T) {
		stub.body = `{"module":{"owner":"acme","name":"catalog"},"versions":[{"version":"v1.0.0"}]}`
		idx, err := c.Module(ctx, "acme/catalog")
		if err != nil {
			t.Fatal(err)
		}
		if len(idx.Versions) != 1 {
			t.Errorf("index = %+v", idx)
		}
		if !strings.Contains(stub.lastPath, "/api/v1/modules/acme/catalog") {
			t.Errorf("path = %q", stub.lastPath)
		}
	})

	t.Run("schema url and download", func(t *testing.T) {
		stub.body = "archive bytes"
		body, err := c.DownloadSchema(ctx, "acme/catalog", "")
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "archive bytes" {
			t.Errorf("body = %q", body)
		}
		// An empty version means latest, which is what buf breaking wants.
		if !strings.Contains(stub.lastPath, "/versions/latest/schema.zip") {
			t.Errorf("path = %q", stub.lastPath)
		}
		if url := c.SchemaURL("acme/catalog", "v1.0.0"); !strings.HasSuffix(url, "/versions/v1.0.0/schema.zip") {
			t.Errorf("SchemaURL = %q", url)
		}
	})

	t.Run("label and yank", func(t *testing.T) {
		stub.body = `{}`
		if err := c.SetLabel(ctx, "acme/catalog", "stable", "v1.0.0"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stub.lastPath, "/labels/stable") {
			t.Errorf("path = %q", stub.lastPath)
		}
		if err := c.SetYanked(ctx, "acme/catalog", "v1.0.0", true, "bad build"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stub.lastPath, "/versions/v1.0.0/yank") {
			t.Errorf("path = %q", stub.lastPath)
		}
	})

	t.Run("error bodies reach the caller", func(t *testing.T) {
		stub.status = http.StatusForbidden
		stub.body = `token "ci" may not write to module other/thing`
		_, err := c.Whoami(ctx)
		if err == nil || !strings.Contains(err.Error(), "may not write to module") {
			t.Errorf("err = %v, want the server's message", err)
		}
		stub.status = 0
	})
}
