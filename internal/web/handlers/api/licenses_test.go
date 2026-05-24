// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type apiLicense struct {
	Key    string `json:"key"`
	SPDXID string `json:"spdx_id"`
	Name   string `json:"name"`
}

// TestLicenses_ListReturnsCatalog pins audit-I35 discovery: the
// /licenses endpoint exposes the curated SPDX list with lowercase
// keys + SPDX casing, so the CLI can resolve user input without
// guessing the canonical case.
func TestLicenses_ListReturnsCatalog(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/licenses", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	var listed []apiLicense
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rr.Body.String())
	}
	if len(listed) < 10 {
		t.Fatalf("expected ≥10 licenses, got %d", len(listed))
	}

	// Every row carries lowercase Key + uppercase-ish SPDXID + a Name.
	wantMITSeen := false
	for _, l := range listed {
		if l.Key == "" || l.SPDXID == "" || l.Name == "" {
			t.Errorf("incomplete row: %+v", l)
		}
		if l.Key != strings.ToLower(l.SPDXID) {
			t.Errorf("Key %q is not lowercase of SPDXID %q", l.Key, l.SPDXID)
		}
		if l.SPDXID == "MIT" {
			wantMITSeen = true
			if l.Name != "MIT License" {
				t.Errorf("MIT row: got Name=%q, want %q", l.Name, "MIT License")
			}
		}
	}
	if !wantMITSeen {
		t.Error("MIT entry missing from catalog")
	}
}

// TestLicenses_GetCaseInsensitive pins the case-insensitive lookup.
// `mit`, `MIT`, `Mit` all resolve to the canonical row.
func TestLicenses_GetCaseInsensitive(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	for _, raw := range []string{"mit", "MIT", "Mit"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/licenses/"+raw, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("key=%q: status %d, want 200; body=%s", raw, rr.Code, rr.Body.String())
			continue
		}
		var got apiLicense
		_ = json.Unmarshal(rr.Body.Bytes(), &got)
		if got.SPDXID != "MIT" {
			t.Errorf("key=%q: got SPDXID=%q, want MIT", raw, got.SPDXID)
		}
	}
}

func TestLicenses_GetUnknownReturns404(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/licenses/totally-bogus", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}
