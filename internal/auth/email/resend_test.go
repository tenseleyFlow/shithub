// SPDX-License-Identifier: AGPL-3.0-or-later

package email

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResendSender_Send_SuccessShapesRequest(t *testing.T) {
	t.Parallel()
	var (
		gotMethod string
		gotPath   string
		gotAuth   string
		gotCT     string
		gotBody   resendPayload
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"00000000-0000-0000-0000-000000000000"}`))
	}))
	defer srv.Close()

	s := &ResendSender{
		APIKey:   "re_test_secret",
		From:     "noreply@shithub.sh",
		Endpoint: srv.URL,
	}
	err := s.Send(context.Background(), Message{
		To: "alice@example.com", Subject: "hi", HTML: "<b>hi</b>", Text: "hi",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/" {
		t.Errorf("path = %q, want /", gotPath)
	}
	if gotAuth != "Bearer re_test_secret" {
		t.Errorf("Authorization = %q, want Bearer re_test_secret", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	if gotBody.From != "noreply@shithub.sh" {
		t.Errorf("body.From = %q, want default From", gotBody.From)
	}
	if gotBody.To != "alice@example.com" || gotBody.Subject != "hi" ||
		gotBody.HTML != "<b>hi</b>" || gotBody.Text != "hi" {
		t.Errorf("body fields wrong: %+v", gotBody)
	}
}

func TestResendSender_Send_PerMessageFromOverridesDefault(t *testing.T) {
	t.Parallel()
	var gotFrom string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p resendPayload
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &p)
		gotFrom = p.From
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := &ResendSender{APIKey: "k", From: "default@x", Endpoint: srv.URL}
	if err := s.Send(context.Background(), Message{
		From: "override@x", To: "a@x", Subject: "s", HTML: "H", Text: "T",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotFrom != "override@x" {
		t.Errorf("From = %q, want override@x", gotFrom)
	}
}

func TestResendSender_Send_PropagatesAPIError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"name":"missing_api_key","message":"API key is missing"}`))
	}))
	defer srv.Close()

	s := &ResendSender{APIKey: "bad", From: "noreply@x", Endpoint: srv.URL}
	err := s.Send(context.Background(), Message{To: "a@x", Subject: "s", HTML: "H", Text: "T"})
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error missing status code: %v", err)
	}
	if !strings.Contains(err.Error(), "missing_api_key") {
		t.Errorf("error missing API body snippet: %v", err)
	}
}
