// SPDX-License-Identifier: AGPL-3.0-or-later

// Package api is the shithubd-runner client for the S41c runner HTTP API.
package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	BaseURL     string
	RunnerToken string
	HTTPClient  *http.Client
}

type Client struct {
	base        *url.URL
	runnerToken string
	http        *http.Client
}

func New(cfg Config) (*Client, error) {
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("runner api: invalid base URL %q", cfg.BaseURL)
	}
	if strings.TrimSpace(cfg.RunnerToken) == "" {
		return nil, fmt.Errorf("runner api: runner token is required")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{base: base, runnerToken: strings.TrimSpace(cfg.RunnerToken), http: hc}, nil
}

type HeartbeatRequest struct {
	Labels   []string `json:"labels"`
	Capacity int      `json:"capacity"`
}

type Claim struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	Job       Job       `json:"job"`
}

type Job struct {
	ID             int64             `json:"id"`
	RunID          int64             `json:"run_id"`
	RepoID         int64             `json:"repo_id"`
	RunIndex       int64             `json:"run_index"`
	WorkflowFile   string            `json:"workflow_file"`
	WorkflowName   string            `json:"workflow_name"`
	HeadSHA        string            `json:"head_sha"`
	HeadRef        string            `json:"head_ref"`
	Event          string            `json:"event"`
	JobKey         string            `json:"job_key"`
	JobName        string            `json:"job_name"`
	RunsOn         string            `json:"runs_on"`
	Needs          []string          `json:"needs"`
	If             string            `json:"if"`
	TimeoutMinutes int32             `json:"timeout_minutes"`
	Permissions    json.RawMessage   `json:"permissions"`
	Env            map[string]string `json:"env"`
	Steps          []Step            `json:"steps"`
}

type Step struct {
	ID               int64             `json:"id"`
	Index            int32             `json:"index"`
	StepID           string            `json:"step_id"`
	Name             string            `json:"name"`
	If               string            `json:"if"`
	Run              string            `json:"run"`
	Uses             string            `json:"uses"`
	WorkingDirectory string            `json:"working_directory"`
	Env              map[string]string `json:"env"`
	With             map[string]string `json:"with"`
	ContinueOnError  bool              `json:"continue_on_error"`
}

type StatusRequest struct {
	Status      string    `json:"status"`
	Conclusion  string    `json:"conclusion,omitempty"`
	StartedAt   time.Time `json:"-"`
	CompletedAt time.Time `json:"-"`
}

func (r StatusRequest) MarshalJSON() ([]byte, error) {
	type wire struct {
		Status      string `json:"status"`
		Conclusion  string `json:"conclusion,omitempty"`
		StartedAt   string `json:"started_at,omitempty"`
		CompletedAt string `json:"completed_at,omitempty"`
	}
	out := wire{Status: r.Status, Conclusion: r.Conclusion}
	if !r.StartedAt.IsZero() {
		out.StartedAt = r.StartedAt.UTC().Format(time.RFC3339Nano)
	}
	if !r.CompletedAt.IsZero() {
		out.CompletedAt = r.CompletedAt.UTC().Format(time.RFC3339Nano)
	}
	return json.Marshal(out)
}

type StatusResponse struct {
	Status             string    `json:"status"`
	Conclusion         *string   `json:"conclusion"`
	RunStatus          string    `json:"run_status,omitempty"`
	RunConclusion      string    `json:"run_conclusion,omitempty"`
	NextToken          string    `json:"next_token,omitempty"`
	NextTokenExpiresAt time.Time `json:"next_token_expires_at,omitempty"`
}

type LogRequest struct {
	Seq    int32  `json:"seq"`
	Chunk  []byte `json:"-"`
	StepID int64  `json:"step_id,omitempty"`
}

func (r LogRequest) MarshalJSON() ([]byte, error) {
	type wire struct {
		Seq    int32  `json:"seq"`
		Chunk  string `json:"chunk"`
		StepID int64  `json:"step_id,omitempty"`
	}
	return json.Marshal(wire{
		Seq:    r.Seq,
		Chunk:  base64.StdEncoding.EncodeToString(r.Chunk),
		StepID: r.StepID,
	})
}

type LogResponse struct {
	Accepted           bool      `json:"accepted"`
	NextToken          string    `json:"next_token"`
	NextTokenExpiresAt time.Time `json:"next_token_expires_at"`
}

type StepStatusResponse struct {
	Status             string    `json:"status"`
	Conclusion         *string   `json:"conclusion"`
	NextToken          string    `json:"next_token,omitempty"`
	NextTokenExpiresAt time.Time `json:"next_token_expires_at,omitempty"`
}

type CancelCheckResponse struct {
	Cancelled          bool      `json:"cancelled"`
	NextToken          string    `json:"next_token"`
	NextTokenExpiresAt time.Time `json:"next_token_expires_at"`
}

func (c *Client) Heartbeat(ctx context.Context, req HeartbeatRequest) (*Claim, error) {
	var claim Claim
	status, err := c.do(ctx, http.MethodPost, "/api/v1/runners/heartbeat", c.runnerToken, req, &claim)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNoContent {
		return nil, nil
	}
	return &claim, nil
}

func (c *Client) UpdateStatus(ctx context.Context, jobID int64, token string, req StatusRequest) (StatusResponse, error) {
	var out StatusResponse
	_, err := c.do(ctx, http.MethodPost, jobPath(jobID, "status"), token, req, &out)
	return out, err
}

func (c *Client) UpdateStepStatus(ctx context.Context, jobID, stepID int64, token string, req StatusRequest) (StepStatusResponse, error) {
	var out StepStatusResponse
	_, err := c.do(ctx, http.MethodPost, jobPath(jobID, "steps/"+strconv.FormatInt(stepID, 10)+"/status"), token, req, &out)
	return out, err
}

func (c *Client) AppendLog(ctx context.Context, jobID int64, token string, req LogRequest) (LogResponse, error) {
	var out LogResponse
	_, err := c.do(ctx, http.MethodPost, jobPath(jobID, "logs"), token, req, &out)
	return out, err
}

func (c *Client) CancelCheck(ctx context.Context, jobID int64, token string) (CancelCheckResponse, error) {
	var out CancelCheckResponse
	_, err := c.do(ctx, http.MethodPost, jobPath(jobID, "cancel-check"), token, map[string]string{}, &out)
	return out, err
}

func jobPath(jobID int64, suffix string) string {
	return "/api/v1/jobs/" + strconv.FormatInt(jobID, 10) + "/" + suffix
}

func (c *Client) do(ctx context.Context, method, path, bearer string, body, out any) (int, error) {
	var r io.Reader
	if body != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return 0, fmt.Errorf("runner api: encode %s %s: %w", method, path, err)
		}
		r = &buf
	}
	u := c.base.ResolveReference(&url.URL{Path: path})
	req, err := http.NewRequestWithContext(ctx, method, u.String(), r)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if strings.TrimSpace(bearer) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(bearer))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("runner api: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return resp.StatusCode, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return resp.StatusCode, fmt.Errorf("runner api: %s %s returned %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return resp.StatusCode, fmt.Errorf("runner api: decode %s %s: %w", method, path, err)
	}
	return resp.StatusCode, nil
}
