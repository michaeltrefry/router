//go:build smoke

package smoke

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// routerKeyHeader is how the installer's `models` command authenticates.
// Must match internal/auth/request_token.go:RouterKeyHeader.
const routerKeyHeader = "X-Weave-Router-Key"

type selectionModel struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
	Enabled  bool   `json:"enabled"`
	Local    bool   `json:"local"`
}

// adminCall sends a model-selection request with the seeded router key, the
// same credential the installer reads from an install.
func adminCall(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, cfg.BaseURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	req.Header.Set(routerKeyHeader, cfg.RouterKey)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, path, err)
	}
	return resp.StatusCode, raw
}

func listSelectionModels(t *testing.T) map[string]selectionModel {
	t.Helper()
	status, raw := adminCall(t, http.MethodGet, "/admin/v1/models", "")
	if status != http.StatusOK {
		t.Fatalf("GET /admin/v1/models: want 200, got %d; body: %s", status, truncate(raw, 400))
	}
	var rows []selectionModel
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	out := make(map[string]selectionModel, len(rows))
	for _, r := range rows {
		out[r.Model] = r
	}
	return out
}

// TestModelSelection drives the API behind `install.sh models` against the
// self-hosted router: the listing includes the configured local model, a
// disable lands on the dashboard's exclusion list and refuses a forced turn,
// and the preferred ranking round-trips.
func TestModelSelection(t *testing.T) {
	models := listSelectionModels(t)
	local, ok := models[localModelID]
	if !ok || local.Provider != localModelProvider || !local.Local || !local.Enabled {
		t.Fatalf("want enabled local model %s on %s, got %+v (present=%v)", localModelID, localModelProvider, local, ok)
	}
	if _, ok := models[cfg.PinModel]; !ok {
		t.Fatalf("want catalog model %s listed", cfg.PinModel)
	}

	status, raw := adminCall(t, http.MethodGet, "/admin/v1/providers", "")
	if status != http.StatusOK || !bytes.Contains(raw, []byte(`"provider":"`+localModelProvider+`"`)) {
		t.Fatalf("GET /admin/v1/providers: want 200 listing %s, got %d; body: %s", localModelProvider, status, truncate(raw, 400))
	}

	t.Run("disable refuses a forced turn until re-enabled", func(t *testing.T) {
		item := `{"model":"` + localModelID + `"}`
		t.Cleanup(func() { adminCall(t, http.MethodPost, "/admin/v1/excluded-models/remove", item) })
		if status, raw := adminCall(t, http.MethodPost, "/admin/v1/excluded-models", item); status != http.StatusOK {
			t.Fatalf("disable: want 200, got %d; body: %s", status, truncate(raw, 400))
		}

		status, raw := adminCall(t, http.MethodGet, "/admin/v1/excluded-models", "")
		var dashboard struct {
			Excluded []string `json:"excluded"`
		}
		if status != http.StatusOK || json.Unmarshal(raw, &dashboard) != nil || !slices.Contains(dashboard.Excluded, localModelID) {
			t.Fatalf("dashboard exclusion list: want %s, got %d; body: %s", localModelID, status, truncate(raw, 400))
		}
		if listSelectionModels(t)[localModelID].Enabled {
			t.Fatalf("want %s disabled in the listing", localModelID)
		}

		body := newRequest("smoke-selection-excluded").tokens(64).text("Reply with exactly the word: ok").build(t)
		r := callModel(t, body, localModelID)
		if r.status != http.StatusBadRequest || !bytes.Contains(r.body, []byte("excluded")) {
			t.Fatalf("forced turn on a disabled model: want 400 naming the exclusion, got %d; body: %s", r.status, truncate(r.body, 400))
		}

		if status, raw := adminCall(t, http.MethodPost, "/admin/v1/excluded-models/remove", item); status != http.StatusOK {
			t.Fatalf("enable: want 200, got %d; body: %s", status, truncate(raw, 400))
		}
		if !listSelectionModels(t)[localModelID].Enabled {
			t.Fatalf("want %s enabled again", localModelID)
		}
	})

	t.Run("preferred ranking round-trips", func(t *testing.T) {
		t.Cleanup(func() { adminCall(t, http.MethodPut, "/admin/v1/preferred-models", `{"preferred":[]}`) })
		want := `{"preferred":["` + localModelID + `","` + cfg.PinModel + `"]}`
		if status, raw := adminCall(t, http.MethodPut, "/admin/v1/preferred-models", want); status != http.StatusOK {
			t.Fatalf("prefer: want 200, got %d; body: %s", status, truncate(raw, 400))
		}
		status, raw := adminCall(t, http.MethodGet, "/admin/v1/preferred-models", "")
		if status != http.StatusOK || strings.TrimSpace(string(raw)) != want {
			t.Fatalf("GET preferred: want %s, got %d %s", want, status, truncate(raw, 400))
		}
	})
}
