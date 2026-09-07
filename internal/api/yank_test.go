package api_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/smoreg/protobuffet/internal/client"
	"github.com/smoreg/protobuffet/internal/config"
)

// TestYankHidesButKeepsDownloadable is the whole point of yanking: a bad
// release stops being handed out, and everyone who already pinned it keeps
// building.
func TestYankHidesButKeepsDownloadable(t *testing.T) {
	ts := gateServer(t, "exit 0\n", config.BreakingOff, config.PolicyStrict)
	admin := client.New(ts.URL, "admin-token")
	ctx := context.Background()

	for _, v := range []string{"v1.0.0", "v1.1.0"} {
		man, files := buildFixture(t, v)
		if _, ok, err := admin.Publish(ctx, man, files); err != nil || !ok {
			t.Fatalf("publish %s: %v %v", v, ok, err)
		}
	}
	if err := admin.SetLabel(ctx, "acme/catalog", "main", "v1.1.0"); err != nil {
		t.Fatal(err)
	}

	if err := admin.SetYanked(ctx, "acme/catalog", "v1.1.0", true, "leaked a secret"); err != nil {
		t.Fatal(err)
	}

	t.Run("go proxy hides it", func(t *testing.T) {
		list := get(t, ts.URL+"/go/"+goModPath+"/@v/list")
		if strings.Contains(list, "v1.1.0") {
			t.Errorf("yanked version listed: %q", list)
		}
		if !strings.Contains(list, "v1.0.0") {
			t.Errorf("good version missing: %q", list)
		}
		// @latest must fall back to the newest version that is not yanked.
		if latest := get(t, ts.URL+"/go/"+goModPath+"/@latest"); !strings.Contains(latest, "v1.0.0") {
			t.Errorf("@latest = %q", latest)
		}
		// The pinned download still works.
		if body := get(t, ts.URL+"/go/"+goModPath+"/@v/v1.1.0.zip"); len(body) < 100 {
			t.Errorf("yanked zip is %d bytes", len(body))
		}
		if info := get(t, ts.URL+"/go/"+goModPath+"/@v/v1.1.0.info"); !strings.Contains(info, "v1.1.0") {
			t.Errorf("yanked info = %q", info)
		}
	})

	t.Run("composer and npm hide it", func(t *testing.T) {
		if pkgs := get(t, ts.URL+"/composer/p2/acme/catalog-proto.json"); strings.Contains(pkgs, `"1.1.0"`) {
			t.Error("composer still lists the yanked version")
		}
		if doc := get(t, ts.URL+"/npm/@acme%2fcatalog"); strings.Contains(doc, `"1.1.0"`) {
			t.Error("npm still lists the yanked version")
		}
	})

	t.Run("pypi marks it yanked per PEP 592", func(t *testing.T) {
		page := get(t, ts.URL+"/pypi/simple/acme-catalog-proto/")
		if !strings.Contains(page, `data-yanked="leaked a secret"`) {
			t.Errorf("pypi page = %q", page)
		}
	})

	t.Run("labels pointing at it are dropped", func(t *testing.T) {
		idx, err := admin.Module(ctx, "acme/catalog")
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := idx.Labels["main"]; ok {
			t.Error("a label still points at the yanked version")
		}
	})

	t.Run("unyank restores it", func(t *testing.T) {
		if err := admin.SetYanked(ctx, "acme/catalog", "v1.1.0", false, ""); err != nil {
			t.Fatal(err)
		}
		if list := get(t, ts.URL+"/go/"+goModPath+"/@v/list"); !strings.Contains(list, "v1.1.0") {
			t.Errorf("unyanked version still hidden: %q", list)
		}
	})
}

func TestYankRequiresAdminAndAValidVersion(t *testing.T) {
	ts := gateServer(t, "exit 0\n", config.BreakingOff, config.PolicyStrict)
	ctx := context.Background()

	man, files := buildFixture(t, "v1.0.0")
	if _, ok, err := client.New(ts.URL, testToken).Publish(ctx, man, files); err != nil || !ok {
		t.Fatalf("publish: %v %v", ok, err)
	}
	// testToken carries only "write".
	if err := client.New(ts.URL, testToken).SetYanked(ctx, "acme/catalog", "v1.0.0", true, ""); err == nil {
		t.Error("a write token yanked a version")
	}
	admin := client.New(ts.URL, "admin-token")
	if err := admin.SetYanked(ctx, "acme/catalog", "v9.9.9", true, ""); err == nil {
		t.Error("yanked a version that does not exist")
	}
	if err := admin.SetYanked(ctx, "acme/catalog", "v1.0.0", true, "bad build"); err != nil {
		t.Fatal(err)
	}
	// The reason is recorded, not just the flag.
	body := get(t, ts.URL+"/api/v1/modules/acme/catalog")
	var idx struct {
		Versions []struct {
			Version    string
			Yanked     bool
			YankReason string `json:"yank_reason"`
			YankedBy   string `json:"yanked_by"`
		}
	}
	if err := json.Unmarshal([]byte(body), &idx); err != nil {
		t.Fatal(err)
	}
	v := idx.Versions[0]
	if !v.Yanked || v.YankReason != "bad build" || v.YankedBy != "boss" {
		t.Errorf("version = %+v", v)
	}
}
