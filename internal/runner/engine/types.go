// SPDX-License-Identifier: AGPL-3.0-or-later

// Package engine defines the runner execution boundary.
package engine

import (
	"context"
	"encoding/json"
	"time"
)

const (
	ConclusionSuccess   = "success"
	ConclusionFailure   = "failure"
	ConclusionCancelled = "cancelled"
	ConclusionTimedOut  = "timed_out"
)

type Engine interface {
	Execute(ctx context.Context, job Job) (Outcome, error)
	StreamLogs(ctx context.Context, jobID int64) (<-chan LogChunk, error)
	Cancel(ctx context.Context, jobID int64) error
}

// EventStreamer is an optional engine capability for preserving runner API
// ordering between final log chunks and step completion updates.
type EventStreamer interface {
	StreamEvents(ctx context.Context, jobID int64) (<-chan Event, error)
}

type Job struct {
	ID             int64
	RunID          int64
	RepoID         int64
	RunIndex       int64
	WorkflowFile   string
	WorkflowName   string
	HeadSHA        string
	HeadRef        string
	Event          string
	EventPayload   map[string]any
	JobKey         string
	JobName        string
	RunsOn         string
	Needs          []string
	If             string
	TimeoutMinutes int32
	Permissions    json.RawMessage
	Env            map[string]string
	Steps          []Step
	WorkspaceDir   string
	Image          string
	MaskValues     []string
}

type Step struct {
	ID               int64
	Index            int32
	StepID           string
	Name             string
	If               string
	Run              string
	Uses             string
	WorkingDirectory string
	Env              map[string]string
	With             map[string]string
	ContinueOnError  bool
}

type Outcome struct {
	Conclusion   string
	StartedAt    time.Time
	CompletedAt  time.Time
	StepOutcomes []StepOutcome
}

type StepOutcome struct {
	StepID      int64
	Status      string
	Conclusion  string
	StartedAt   time.Time
	CompletedAt time.Time
}

type LogChunk struct {
	JobID  int64
	StepID int64
	Seq    int32
	Chunk  []byte
}

type Event struct {
	Log  *LogChunk
	Step *StepOutcome
}
