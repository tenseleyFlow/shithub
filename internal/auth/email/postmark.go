// SPDX-License-Identifier: AGPL-3.0-or-later

package email

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// PostmarkSender posts messages to the Postmark transactional API. The
// integration is intentionally thin: one HTTP POST per message, no
// templates, no batching. Postmark's templating story stays out-of-band.
type PostmarkSender struct {
	ServerToken string // X-Postmark-Server-Token
	From        string // verified sender address
	HTTP        *http.Client
}

// postmarkAPI is the canonical endpoint. Hard-coded — Postmark is a SaaS,
// not something operators self-host.
const postmarkAPI = "https://api.postmarkapp.com/email"

// Send implements Sender.
func (p *PostmarkSender) Send(ctx context.Context, m Message) error {
	if m.From == "" {
		m.From = p.From
	}
	payload := map[string]string{
		"From":     m.From,
		"To":       m.To,
		"Subject":  m.Subject,
		"HtmlBody": m.HTML,
		"TextBody": m.Text,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("postmark: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, postmarkAPI, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("postmark: request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Postmark-Server-Token", p.ServerToken)

	client := p.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("postmark: send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("postmark: status %d", resp.StatusCode)
	}
	return nil
}
