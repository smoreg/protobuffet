package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/smoreg/protobuffet/internal/client"
	"github.com/smoreg/protobuffet/internal/config"
)

func TestAuditLogRecordsWhoDidWhat(t *testing.T) {
	ts := gateServer(t, bufFindsBreakage, config.BreakingRequired, config.PolicyStrict)
	admin := client.New(ts.URL, "admin-token")
	ci := client.New(ts.URL, testToken)
	ctx := context.Background()

	man, files := buildFixture(t, "v1.0.0")
	if _, ok, err := ci.Publish(ctx, man, files); err != nil || !ok {
		t.Fatalf("publish: %v %v", ok, err)
	}
	// A refused publish must leave a trace too: that is the event someone
	// goes looking for afterwards.
	man2, files2 := buildFixture(t, "v1.1.0")
	if _, ok, _ := ci.Publish(ctx, man2, files2); ok {
		t.Fatal("breaking change published")
	}
	if err := admin.SetLabel(ctx, "acme/catalog", "stable", "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := admin.SetYanked(ctx, "acme/catalog", "v1.0.0", true, "oops"); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/audit", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer admin-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("audit status = %d", resp.StatusCode)
	}
	var events []struct {
		Subject   string `json:"subject"`
		Action    string `json:"action"`
		Module    string `json:"module"`
		Version   string `json:"version"`
		Label     string `json:"label"`
		Result    string `json:"result"`
		RequestID string `json:"request_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4: %+v", len(events), events)
	}
	// Newest first.
	if events[0].Action != "yank" || events[0].Subject != "boss" {
		t.Errorf("newest event = %+v", events[0])
	}
	var sawRefusal bool
	for _, e := range events {
		if e.Action == "publish" && e.Result == "refused-breaking" {
			sawRefusal = true
			if e.Subject != "ci" {
				t.Errorf("refusal recorded against %q", e.Subject)
			}
		}
		if e.RequestID == "" {
			t.Errorf("event without a request id: %+v", e)
		}
	}
	if !sawRefusal {
		t.Error("a refused publish left no audit trail")
	}
}

func TestAuditRequiresAdmin(t *testing.T) {
	ts := gateServer(t, "exit 0\n", config.BreakingOff, config.PolicyStrict)
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/audit", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken) // write scope only
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestAuditLogSurvivesATruncatedLine(t *testing.T) {
	ts := gateServer(t, "exit 0\n", config.BreakingOff, config.PolicyStrict)
	admin := client.New(ts.URL, "admin-token")
	man, files := buildFixture(t, "v1.0.0")
	if _, ok, err := admin.Publish(context.Background(), man, files); err != nil || !ok {
		t.Fatalf("publish: %v %v", ok, err)
	}
	// A crash mid-write leaves a partial line; it must not hide the rest.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/audit?limit=5", nil)
	req.Header.Set("Authorization", "Bearer admin-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	if !strings.Contains(string(body[:n]), "publish") {
		t.Errorf("audit tail = %q", body[:n])
	}
}
