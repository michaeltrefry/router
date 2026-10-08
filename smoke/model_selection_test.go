//go:build smoke

package smoke

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
)

// routerKeyHeader is how the installer's `models` command authenticates.
// Must match internal/auth/request_token.go:RouterKeyHeader.
const routerKeyHeader = "X-Weave-Router-Key"

// adminSessionCookie must match internal/auth/admin.go:AdminSessionCookieName.
const adminSessionCookie = "router_admin_session"

type selectionModel struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
	Enabled  bool   `json:"enabled"`
	Local    bool   `json:"local"`
}

var (
	dashboardOnce   sync.Once
	dashboardCookie *http.Cookie
)

// dashboardSession logs in with the dashboard password once per run; every
// model-selection write needs the cookie it returns.
func dashboardSession(t *testing.T) *http.Cookie {
	t.Helper()
	dashboardOnce.Do(func() {
		if cfg.AdminPassword == "" {
			return
		}
		body, _ := json.Marshal(map[string]string{"password": cfg.AdminPassword})
		resp, err := httpClient.Post(cfg.BaseURL+"/admin/v1/auth/login", "application/json", bytes.NewReader(body))
		if err != nil {
			return
		}
		defer resp.Body.Close()
		for _, c := range resp.Cookies() {
			if c.Name == adminSessionCookie {
				dashboardCookie = c
			}
		}
	})
	if dashboardCookie == nil {
		t.Fatalf("dashboard login failed; SMOKE_ADMIN_PASSWORD must match the router's ROUTER_ADMIN_PASSWORD")
	}
	return dashboardCookie
}

func selectionCall(t *testing.T, method, path, body string, auth func(*http.Request)) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, cfg.BaseURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	auth(req)
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

// keyCall sends a request with the seeded router key, the credential the
// installer reads from an install.
func keyCall(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	return selectionCall(t, method, path, body, func(r *http.Request) { r.Header.Set(routerKeyHeader, cfg.RouterKey) })
}

// dashboardCall sends a request the way the dashboard does.
func dashboardCall(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	cookie := dashboardSession(t)
	return selectionCall(t, method, path, body, func(r *http.Request) { r.AddCookie(cookie) })
}

func listSelectionModels(t *testing.T) map[string]selectionModel {
	t.Helper()
	status, raw := keyCall(t, http.MethodGet, "/admin/v1/models", "")
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
// self-hosted router: the router key reads the selection, including the
// configured local model, but every write is dashboard-only; a dashboard
// disable lands on the exclusion list and refuses a forced turn; per-item
// edits keep the stored order; and no exclusion may leave nothing routable.
func TestModelSelection(t *testing.T) {
	models := listSelectionModels(t)
	local, ok := models[localModelID]
	if !ok || local.Provider != localModelProvider || !local.Local || !local.Enabled {
		t.Fatalf("want enabled local model %s on %s, got %+v (present=%v)", localModelID, localModelProvider, local, ok)
	}
	if _, ok := models[cfg.PinModel]; !ok {
		t.Fatalf("want catalog model %s listed", cfg.PinModel)
	}

	status, raw := keyCall(t, http.MethodGet, "/admin/v1/providers", "")
	if status != http.StatusOK || !bytes.Contains(raw, []byte(`"provider":"`+localModelProvider+`"`)) {
		t.Fatalf("GET /admin/v1/providers: want 200 listing %s, got %d; body: %s", localModelProvider, status, truncate(raw, 400))
	}

	t.Run("a router key cannot write", func(t *testing.T) {
		t.Cleanup(func() {
			dashboardCall(t, http.MethodPut, "/admin/v1/excluded-models", `{"excluded":[]}`)
			dashboardCall(t, http.MethodPut, "/admin/v1/excluded-providers", `{"excluded":[]}`)
			dashboardCall(t, http.MethodPut, "/admin/v1/preferred-models", `{"preferred":[]}`)
		})
		for _, w := range []struct{ method, path, body string }{
			{http.MethodPost, "/admin/v1/excluded-models", `{"model":"` + localModelID + `"}`},
			{http.MethodPut, "/admin/v1/preferred-models", `{"preferred":["` + localModelID + `"]}`},
			{http.MethodPost, "/admin/v1/excluded-providers", `{"provider":"` + localModelProvider + `"}`},
		} {
			status, raw := keyCall(t, w.method, w.path, w.body)
			if status != http.StatusForbidden || !bytes.Contains(raw, []byte("/ui/settings/models")) {
				t.Fatalf("%s %s with a router key: want 403 naming the dashboard, got %d; body: %s", w.method, w.path, status, truncate(raw, 400))
			}
		}
		if !listSelectionModels(t)[localModelID].Enabled {
			t.Fatalf("a refused router-key write changed the selection")
		}
	})

	t.Run("dashboard disable refuses a forced turn until re-enabled", func(t *testing.T) {
		item := `{"model":"` + localModelID + `"}`
		t.Cleanup(func() { dashboardCall(t, http.MethodPost, "/admin/v1/excluded-models/remove", item) })
		if status, raw := dashboardCall(t, http.MethodPost, "/admin/v1/excluded-models", item); status != http.StatusOK {
			t.Fatalf("disable: want 200, got %d; body: %s", status, truncate(raw, 400))
		}

		status, raw := keyCall(t, http.MethodGet, "/admin/v1/excluded-models", "")
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

		if status, raw := dashboardCall(t, http.MethodPost, "/admin/v1/excluded-models/remove", item); status != http.StatusOK {
			t.Fatalf("enable: want 200, got %d; body: %s", status, truncate(raw, 400))
		}
		if !listSelectionModels(t)[localModelID].Enabled {
			t.Fatalf("want %s enabled again", localModelID)
		}
	})

	t.Run("per-item edits keep the stored ranking order", func(t *testing.T) {
		t.Cleanup(func() { dashboardCall(t, http.MethodPut, "/admin/v1/preferred-models", `{"preferred":[]}`) })
		if status, raw := dashboardCall(t, http.MethodPut, "/admin/v1/preferred-models", `{"preferred":["`+localModelID+`","`+cfg.PinModel+`"]}`); status != http.StatusOK {
			t.Fatalf("prefer: want 200, got %d; body: %s", status, truncate(raw, 400))
		}
		var third string
		for id, m := range models {
			if id != localModelID && id != cfg.PinModel && m.Enabled {
				third = id
				break
			}
		}
		if third == "" {
			t.Fatalf("want a third selectable model, got %v", models)
		}
		for _, add := range []string{third, third} {
			if status, raw := dashboardCall(t, http.MethodPost, "/admin/v1/preferred-models", `{"model":"`+add+`"}`); status != http.StatusOK {
				t.Fatalf("add %s: want 200, got %d; body: %s", add, status, truncate(raw, 400))
			}
		}
		status, raw := dashboardCall(t, http.MethodPost, "/admin/v1/preferred-models/remove", `{"model":"`+localModelID+`"}`)
		want := `{"preferred":["` + cfg.PinModel + `","` + third + `"]}`
		if status != http.StatusOK || strings.TrimSpace(string(raw)) != want {
			t.Fatalf("remove: want 200 %s, got %d %s", want, status, truncate(raw, 400))
		}
		status, raw = keyCall(t, http.MethodGet, "/admin/v1/preferred-models", "")
		if status != http.StatusOK || strings.TrimSpace(string(raw)) != want {
			t.Fatalf("GET preferred: want %s, got %d %s", want, status, truncate(raw, 400))
		}
	})

	// The local provider serves only the local model, so once every other
	// provider is excluded that model is the last thing routable.
	t.Run("no exclusion may leave nothing routable", func(t *testing.T) {
		status, raw := keyCall(t, http.MethodGet, "/admin/v1/excluded-providers", "")
		var listed struct {
			Available []string `json:"available"`
		}
		if status != http.StatusOK || json.Unmarshal(raw, &listed) != nil || !slices.Contains(listed.Available, localModelProvider) || len(listed.Available) < 2 {
			t.Fatalf("GET excluded-providers: want 200 listing %s and another provider, got %d; body: %s", localModelProvider, status, truncate(raw, 400))
		}
		t.Cleanup(func() {
			dashboardCall(t, http.MethodPut, "/admin/v1/excluded-models", `{"excluded":[]}`)
			dashboardCall(t, http.MethodPut, "/admin/v1/excluded-providers", `{"excluded":[]}`)
		})
		const refused = "no model the router can route to"

		every, _ := json.Marshal(map[string][]string{"excluded": listed.Available})
		if status, raw := dashboardCall(t, http.MethodPut, "/admin/v1/excluded-providers", string(every)); status != http.StatusBadRequest || !bytes.Contains(raw, []byte(refused)) {
			t.Fatalf("PUT every provider: want 400, got %d; body: %s", status, truncate(raw, 400))
		}

		for _, p := range listed.Available {
			if p == localModelProvider {
				continue
			}
			if status, raw := dashboardCall(t, http.MethodPost, "/admin/v1/excluded-providers", `{"provider":"`+p+`"}`); status != http.StatusOK {
				t.Fatalf("exclude %s: want 200, got %d; body: %s", p, status, truncate(raw, 400))
			}
		}
		for _, w := range []struct{ method, path, body string }{
			{http.MethodPost, "/admin/v1/excluded-providers", `{"provider":"` + localModelProvider + `"}`},
			{http.MethodPost, "/admin/v1/excluded-models", `{"model":"` + localModelID + `"}`},
			{http.MethodPut, "/admin/v1/excluded-models", `{"excluded":["` + localModelID + `"]}`},
		} {
			if status, raw := dashboardCall(t, w.method, w.path, w.body); status != http.StatusBadRequest || !bytes.Contains(raw, []byte(refused)) {
				t.Fatalf("%s %s %s: want 400, got %d; body: %s", w.method, w.path, w.body, status, truncate(raw, 400))
			}
		}
		if !listSelectionModels(t)[localModelID].Enabled {
			t.Fatalf("a refused exclusion changed the selection")
		}
	})
}
