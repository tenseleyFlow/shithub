// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/actions/secrets"
	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

func TestActionsEnvironments_CRUDAndSecrets(t *testing.T) {
	env := newSecretsTestEnv(t)

	body, _ := json.Marshal(map[string]any{
		"wait_timer":                 7,
		"required_reviewers_enabled": true,
		"prevent_self_review":        true,
		"deployment_branch_policy": map[string]any{
			"protected_branches":     false,
			"custom_branch_policies": true,
		},
		"branch_patterns": []string{"trunk", "release/*"},
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/repos/alice/demo/environments/production", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+env.tokenRW)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("PUT environment: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var created struct {
		Name                       string `json:"name"`
		WaitTimer                  int32  `json:"wait_timer"`
		RequiredReviewersEnabled   bool   `json:"required_reviewers_enabled"`
		PreventSelfReview          bool   `json:"prevent_self_review"`
		DeploymentBranchPolicyMode string `json:"deployment_branch_policy_mode"`
		DeploymentBranchPolicy     struct {
			CustomBranchPolicies bool `json:"custom_branch_policies"`
		} `json:"deployment_branch_policy"`
		BranchPatterns  []string `json:"branch_patterns"`
		ProtectionRules []struct {
			Type              string `json:"type"`
			PreventSelfReview bool   `json:"prevent_self_review"`
			WaitTimer         int32  `json:"wait_timer"`
		} `json:"protection_rules"`
		SecretsURL string `json:"secrets_url"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode environment: %v", err)
	}
	if created.Name != "production" ||
		created.WaitTimer != 7 ||
		!created.RequiredReviewersEnabled ||
		!created.PreventSelfReview ||
		created.DeploymentBranchPolicyMode != "selected" ||
		!created.DeploymentBranchPolicy.CustomBranchPolicies ||
		len(created.BranchPatterns) != 2 ||
		created.SecretsURL == "" {
		t.Fatalf("environment response mismatch: %+v", created)
	}
	if len(created.ProtectionRules) != 2 {
		t.Fatalf("protection rules = %+v", created.ProtectionRules)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/environments", nil)
	req.Header.Set("Authorization", "Bearer "+env.tokenRO)
	rr = httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("LIST environments: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var list struct {
		TotalCount   int              `json:"total_count"`
		Environments []map[string]any `json:"environments"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if list.TotalCount != 1 || list.Environments[0]["name"] != "production" {
		t.Fatalf("list response = %+v", list)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/environments/production/secrets/public-key", nil)
	req.Header.Set("Authorization", "Bearer "+env.tokenRO)
	rr = httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("environment public-key: got %d; body=%s", rr.Code, rr.Body.String())
	}

	putSecret := httptest.NewRequest(http.MethodPut, "/api/v1/repos/alice/demo/environments/production/secrets/DEPLOY_TOKEN", bytes.NewReader(env.encryptedSecretBody(t)))
	putSecret.Header.Set("Authorization", "Bearer "+env.tokenRW)
	putSecret.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	env.router.ServeHTTP(rr, putSecret)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("PUT environment secret: got %d; body=%s", rr.Code, rr.Body.String())
	}
	dbEnv, err := actionsdb.New().GetRepoEnvironmentByName(context.Background(), env.pool, actionsdb.GetRepoEnvironmentByNameParams{
		RepoID: env.repoID,
		Name:   "production",
	})
	if err != nil {
		t.Fatalf("GetRepoEnvironmentByName: %v", err)
	}
	plain, err := secrets.Deps{Pool: env.pool, Box: env.secretBox}.Get(context.Background(), secrets.EnvironmentScope(dbEnv.ID), "DEPLOY_TOKEN")
	if err != nil {
		t.Fatalf("read environment secret: %v", err)
	}
	if string(plain) != "org-secret-value" {
		t.Fatalf("environment secret plaintext = %q", plain)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/environments/production/secrets", nil)
	req.Header.Set("Authorization", "Bearer "+env.tokenRO)
	rr = httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte("DEPLOY_TOKEN")) {
		t.Fatalf("LIST environment secrets: got %d; body=%s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo/environments/production/secrets/DEPLOY_TOKEN", nil)
	req.Header.Set("Authorization", "Bearer "+env.tokenRW)
	rr = httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE environment secret: got %d; body=%s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo/environments/production", nil)
	req.Header.Set("Authorization", "Bearer "+env.tokenRW)
	rr = httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE environment: got %d; body=%s", rr.Code, rr.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/environments/production", nil)
	req.Header.Set("Authorization", "Bearer "+env.tokenRO)
	rr = httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET deleted environment: got %d; body=%s", rr.Code, rr.Body.String())
	}
}

func TestActionsEnvironments_MutationsRequireRepoWrite(t *testing.T) {
	env := newSecretsTestEnv(t)
	body, _ := json.Marshal(map[string]any{"wait_timer": 1})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/repos/alice/demo/environments/production", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+env.tokenRO)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestActionsEnvironments_PrivateOrgWritesRequireTeam(t *testing.T) {
	env := newSecretsTestEnv(t)
	orgID := env.seedOrg(t, "acme")
	if _, err := reposdb.New().CreateRepo(context.Background(), env.pool, reposdb.CreateRepoParams{
		OwnerOrgID:    pgtype.Int8{Int64: orgID, Valid: true},
		Name:          "private-demo",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPrivate,
	}); err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}

	body, _ := json.Marshal(map[string]any{"wait_timer": 1})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/repos/acme/private-demo/environments/production", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+env.tokenRW)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusPaymentRequired {
		t.Fatalf("free private org environment: got %d, want 402; body=%s", rr.Code, rr.Body.String())
	}

	env.activateTeamPlan(t, orgID)
	req = httptest.NewRequest(http.MethodPut, "/api/v1/repos/acme/private-demo/environments/production", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+env.tokenRW)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("team private org environment: got %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
}
