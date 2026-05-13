// SPDX-License-Identifier: AGPL-3.0-or-later

package email

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ResendSender posts messages to the Resend transactional API
// (https://resend.com). Same shape as PostmarkSender — one HTTP POST per
// message, no templating, no batching. Resend's value vs. Postmark is
// near-instant onboarding (no human approval queue), which is why we
// keep both implementations selectable per-deploy.
type ResendSender struct {
	APIKey   string // Bearer token; ops creates this in the Resend dashboard
	From     string // verified sender address; domain must be verified in Resend
	Endpoint string // optional override; defaults to resendAPI for tests
	HTTP     *http.Client
}

// resendAPI is the canonical endpoint. Hard-coded — Resend is SaaS.
const resendAPI = "https://api.resend.com/emails"

// resendPayload is the request body shape. The field names match
// Resend's documented JSON keys (lowercase singular).
type resendPayload struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Subject string `json:"subject"`
	HTML    string `json:"html"`
	Text    string `json:"text"`
}

// Send implements Sender.
func (r *ResendSender) Send(ctx context.Context, m Message) error {
	if m.From == "" {
		m.From = r.From
	}
	body, err := json.Marshal(resendPayload(m))
	if err != nil {
		return fmt.Errorf("resend: marshal: %w", err)
	}

	endpoint := r.Endpoint
	if endpoint == "" {
		endpoint = resendAPI
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("resend: request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.APIKey)

	client := r.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("resend: send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		// Resend returns a JSON error body like {"name":"...", "message":"..."}.
		// Surface a snippet so operators can debug bad keys / unverified
		// domains without re-running with curl.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("resend: status %d: %s", resp.StatusCode, bytes.TrimSpace(snippet))
	}
	return nil
}
