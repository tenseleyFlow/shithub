// SPDX-License-Identifier: AGPL-3.0-or-later

// Package webedit owns repository file mutations initiated from the web UI.
// It uses the same canonical git objects and post-push worker pipeline as
// smart HTTP/SSH pushes, with an update-ref CAS at the end of each commit.
package webedit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/repos/protection"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
	workerdb "github.com/tenseleyFlow/shithub/internal/worker/sqlc"
)

const (
	// MaxTextBytes matches the blob viewer's text threshold.
	MaxTextBytes = 1 * 1024 * 1024
	// MaxUploadFileBytes mirrors GitHub's small web upload envelope closely
	// enough for v1 while preventing accidental large object buffering.
	MaxUploadFileBytes = 10 * 1024 * 1024
	MaxUploadBytes     = 25 * 1024 * 1024
)

var (
	ErrInvalidOperation = errors.New("webedit: invalid operation")
	ErrInvalidPath      = errors.New("webedit: invalid path")
	ErrInvalidBranch    = errors.New("webedit: invalid branch")
	ErrNoVerifiedEmail  = errors.New("webedit: no verified primary email")
	ErrPathExists       = errors.New("webedit: path exists")
	ErrPathNotFound     = errors.New("webedit: path not found")
	ErrUnsupportedEntry = errors.New("webedit: unsupported tree entry")
	ErrBinary           = errors.New("webedit: binary content")
	ErrBlobTooLarge     = errors.New("webedit: blob too large")
	ErrConflict         = errors.New("webedit: branch moved")
	ErrProtected        = errors.New("webedit: protected branch")
)

// Op identifies the mutation the web editor is applying.
type Op string

const (
	OpEdit   Op = "edit"
	OpCreate Op = "create"
	OpRename Op = "rename"
	OpDelete Op = "delete"
	OpUpload Op = "upload"
)

// Deps wires the service from handlers.
type Deps struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
	Now    func() time.Time
}

// File is one uploaded file staged into the commit.
type File struct {
	Path string
	Body []byte
}

// Params describes one web file mutation.
type Params struct {
	GitDir      string
	Repo        reposdb.Repo
	Branch      string
	BaseOID     string
	ActorUserID int64
	RequestID   string

	Op         Op
	SourcePath string
	TargetPath string
	Content    []byte
	Files      []File

	Message     string
	Description string
}

// Result describes the committed ref update.
type Result struct {
	BeforeOID   string
	AfterOID    string
	CommitOID   string
	Ref         string
	PushEventID int64
}

// ValidateFilePath is the repository-file path guard shared by handlers and
// service code. It accepts GitHub-style slash-separated relative paths, but
// rejects traversal, control bytes, directory sentinels, and ambiguous forms.
func ValidateFilePath(p string) error {
	if p == "" || len(p) > 4096 {
		return ErrInvalidPath
	}
	if strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return ErrInvalidPath
	}
	if strings.Contains(p, "\\") || strings.Contains(p, "//") {
		return ErrInvalidPath
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return ErrInvalidPath
		}
		for _, c := range seg {
			if c < 0x20 || c == 0x7f {
				return ErrInvalidPath
			}
		}
	}
	return nil
}

// ValidateDirPath accepts the empty root path and otherwise applies the same
// segment checks as file paths while allowing no trailing slash.
func ValidateDirPath(p string) error {
	if p == "" {
		return nil
	}
	return ValidateFilePath(p)
}

// IsBinary scans the first 8 KiB for a NUL byte.
func IsBinary(b []byte) bool {
	const window = 8192
	if len(b) > window {
		b = b[:window]
	}
	return bytes.IndexByte(b, 0) >= 0
}

// DefaultMessage mirrors GitHub's direct-commit defaults for each operation.
func DefaultMessage(op Op, sourcePath, targetPath string, files []File) string {
	switch op {
	case OpCreate:
		return "Create " + targetPath
	case OpRename:
		return "Rename " + sourcePath + " to " + targetPath
	case OpDelete:
		return "Delete " + sourcePath
	case OpUpload:
		if len(files) == 1 {
			return "Upload " + files[0].Path
		}
		return "Upload files"
	default:
		return "Update " + sourcePath
	}
}

// Commit builds one commit and atomically advances refs/heads/<branch>.
func Commit(ctx context.Context, deps Deps, p Params) (Result, error) {
	if deps.Pool == nil {
		return Result{}, errors.New("webedit: Deps missing Pool")
	}
	if p.GitDir == "" || p.Repo.ID == 0 || p.ActorUserID == 0 {
		return Result{}, errors.New("webedit: Params missing required field")
	}
	if !validBranchName(p.Branch) {
		return Result{}, ErrInvalidBranch
	}
	if err := validateParams(p); err != nil {
		return Result{}, err
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}

	authorName, authorEmail, err := resolveAuthor(ctx, deps.Pool, p.ActorUserID)
	if err != nil {
		return Result{}, err
	}

	ref := "refs/heads/" + p.Branch
	before, err := gitOutput(ctx, p.GitDir, "", "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return Result{}, fmt.Errorf("%w: %s", ErrInvalidBranch, strings.TrimSpace(err.Error()))
	}
	before = strings.TrimSpace(before)
	if p.BaseOID != "" && p.BaseOID != before {
		return Result{}, ErrConflict
	}

	index, err := os.CreateTemp("", "shithub-webedit-index-*")
	if err != nil {
		return Result{}, fmt.Errorf("webedit: temp index: %w", err)
	}
	indexPath := index.Name()
	_ = index.Close()
	defer func() { _ = os.Remove(indexPath) }()

	if _, err := gitOutput(ctx, p.GitDir, indexPath, "read-tree", before); err != nil {
		return Result{}, fmt.Errorf("webedit: read-tree: %w", err)
	}
	if err := applyOperation(ctx, p.GitDir, indexPath, before, p); err != nil {
		return Result{}, err
	}

	tree, err := gitOutput(ctx, p.GitDir, indexPath, "write-tree")
	if err != nil {
		return Result{}, fmt.Errorf("webedit: write-tree: %w", err)
	}
	tree = strings.TrimSpace(tree)
	if tree == "" {
		return Result{}, errors.New("webedit: write-tree returned empty oid")
	}

	message := strings.TrimSpace(p.Message)
	if message == "" {
		message = DefaultMessage(p.Op, p.SourcePath, p.TargetPath, p.Files)
	}
	if desc := strings.TrimSpace(p.Description); desc != "" {
		message += "\n\n" + desc
	}
	commit, err := gitCommitTree(ctx, p.GitDir, tree, before, message, authorName, authorEmail, now())
	if err != nil {
		return Result{}, err
	}
	commit = strings.TrimSpace(commit)

	decision, err := protection.Enforce(ctx, deps.Pool, p.GitDir, p.Repo.ID, protection.Update{
		OldSHA: before,
		NewSHA: commit,
		Ref:    ref,
		Pusher: p.ActorUserID,
	})
	if err != nil {
		return Result{}, fmt.Errorf("webedit: branch protection: %w", err)
	}
	if !decision.Allow {
		return Result{}, fmt.Errorf("%w: %s", ErrProtected, protection.FriendlyMessage(decision))
	}

	if _, err := gitOutput(ctx, p.GitDir, "", "update-ref", ref, commit, before); err != nil {
		return Result{}, classifyUpdateRefError(err)
	}

	eventID, err := enqueuePushProcess(ctx, deps.Pool, p.Repo.ID, p.ActorUserID, before, commit, ref, p.RequestID)
	if err != nil && deps.Logger != nil {
		deps.Logger.WarnContext(ctx, "webedit: enqueue push process after commit", "repo_id", p.Repo.ID, "ref", ref, "commit", commit, "error", err)
	}
	return Result{BeforeOID: before, AfterOID: commit, CommitOID: commit, Ref: ref, PushEventID: eventID}, nil
}

func validateParams(p Params) error {
	switch p.Op {
	case OpEdit:
		if err := ValidateFilePath(p.SourcePath); err != nil {
			return err
		}
		if p.TargetPath == "" {
			p.TargetPath = p.SourcePath
		}
		if err := ValidateFilePath(p.TargetPath); err != nil {
			return err
		}
		if len(p.Content) > MaxTextBytes {
			return ErrBlobTooLarge
		}
		if IsBinary(p.Content) {
			return ErrBinary
		}
	case OpRename:
		if err := ValidateFilePath(p.SourcePath); err != nil {
			return err
		}
		if err := ValidateFilePath(p.TargetPath); err != nil {
			return err
		}
		if p.SourcePath == p.TargetPath {
			return ErrInvalidOperation
		}
		if len(p.Content) > MaxTextBytes {
			return ErrBlobTooLarge
		}
		if IsBinary(p.Content) {
			return ErrBinary
		}
	case OpCreate:
		if err := ValidateFilePath(p.TargetPath); err != nil {
			return err
		}
		if len(p.Content) > MaxTextBytes {
			return ErrBlobTooLarge
		}
		if IsBinary(p.Content) {
			return ErrBinary
		}
	case OpDelete:
		if err := ValidateFilePath(p.SourcePath); err != nil {
			return err
		}
	case OpUpload:
		if len(p.Files) == 0 {
			return ErrInvalidOperation
		}
		seen := map[string]struct{}{}
		for _, f := range p.Files {
			if err := ValidateFilePath(f.Path); err != nil {
				return err
			}
			if _, ok := seen[f.Path]; ok {
				return fmt.Errorf("%w: duplicate path %s", ErrInvalidPath, f.Path)
			}
			seen[f.Path] = struct{}{}
			if len(f.Body) > MaxUploadFileBytes {
				return ErrBlobTooLarge
			}
		}
	default:
		return ErrInvalidOperation
	}
	return nil
}

func applyOperation(ctx context.Context, gitDir, indexPath, before string, p Params) error {
	switch p.Op {
	case OpEdit, OpRename:
		info, ok, err := objectAt(ctx, gitDir, before, p.SourcePath)
		if err != nil {
			return err
		}
		if !ok {
			return ErrPathNotFound
		}
		if info.typ != "blob" || info.mode == "120000" || info.mode == "160000" {
			return ErrUnsupportedEntry
		}
		target := p.TargetPath
		if target == "" {
			target = p.SourcePath
		}
		if target != p.SourcePath {
			if err := ensureParentsAreTrees(ctx, gitDir, before, target); err != nil {
				return err
			}
			if _, ok, err := objectAt(ctx, gitDir, before, target); err != nil {
				return err
			} else if ok {
				return ErrPathExists
			}
			if _, err := gitOutput(ctx, gitDir, indexPath, "update-index", "--force-remove", "--", p.SourcePath); err != nil {
				return fmt.Errorf("webedit: remove source: %w", err)
			}
		}
		return addContent(ctx, gitDir, indexPath, info.mode, target, p.Content)
	case OpCreate:
		if _, ok, err := objectAt(ctx, gitDir, before, p.TargetPath); err != nil {
			return err
		} else if ok {
			return ErrPathExists
		}
		if err := ensureParentsAreTrees(ctx, gitDir, before, p.TargetPath); err != nil {
			return err
		}
		return addContent(ctx, gitDir, indexPath, "100644", p.TargetPath, p.Content)
	case OpDelete:
		info, ok, err := objectAt(ctx, gitDir, before, p.SourcePath)
		if err != nil {
			return err
		}
		if !ok {
			return ErrPathNotFound
		}
		if info.typ != "blob" || info.mode == "120000" || info.mode == "160000" {
			return ErrUnsupportedEntry
		}
		if _, err := gitOutput(ctx, gitDir, indexPath, "update-index", "--force-remove", "--", p.SourcePath); err != nil {
			return fmt.Errorf("webedit: remove source: %w", err)
		}
	case OpUpload:
		for _, f := range p.Files {
			if _, ok, err := objectAt(ctx, gitDir, before, f.Path); err != nil {
				return err
			} else if ok {
				return ErrPathExists
			}
			if err := ensureParentsAreTrees(ctx, gitDir, before, f.Path); err != nil {
				return err
			}
			if err := addContent(ctx, gitDir, indexPath, "100644", f.Path, f.Body); err != nil {
				return err
			}
		}
	}
	return nil
}

func addContent(ctx context.Context, gitDir, indexPath, mode, filePath string, body []byte) error {
	oid, err := gitHashObject(ctx, gitDir, body)
	if err != nil {
		return err
	}
	spec := mode + "," + oid + "," + filePath
	if _, err := gitOutput(ctx, gitDir, indexPath, "update-index", "--add", "--cacheinfo", spec); err != nil {
		return fmt.Errorf("webedit: update-index add %s: %w", filePath, err)
	}
	return nil
}

func ensureParentsAreTrees(ctx context.Context, gitDir, rev, filePath string) error {
	parent := parentPath(filePath)
	for parent != "" {
		info, ok, err := objectAt(ctx, gitDir, rev, parent)
		if err != nil {
			return err
		}
		if ok && info.typ != "tree" {
			return ErrPathExists
		}
		parent = parentPath(parent)
	}
	return nil
}

type objectInfo struct {
	mode string
	typ  string
	oid  string
}

func objectAt(ctx context.Context, gitDir, rev, filePath string) (objectInfo, bool, error) {
	out, err := gitOutput(ctx, gitDir, "", "ls-tree", "-z", rev, "--", filePath)
	if err != nil {
		return objectInfo{}, false, fmt.Errorf("webedit: ls-tree %s: %w", filePath, err)
	}
	out = strings.TrimSuffix(out, "\x00")
	if out == "" {
		return objectInfo{}, false, nil
	}
	meta, _, ok := strings.Cut(out, "\t")
	if !ok {
		return objectInfo{}, false, fmt.Errorf("webedit: bad ls-tree output for %s", filePath)
	}
	parts := strings.Split(meta, " ")
	if len(parts) != 3 {
		return objectInfo{}, false, fmt.Errorf("webedit: bad ls-tree metadata for %s", filePath)
	}
	return objectInfo{mode: parts[0], typ: parts[1], oid: parts[2]}, true, nil
}

func gitHashObject(ctx context.Context, gitDir string, body []byte) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", gitDir, "hash-object", "-w", "--stdin")
	cmd.Stdin = bytes.NewReader(body)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("webedit: hash-object: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func gitCommitTree(ctx context.Context, gitDir, tree, parent, message, name, email string, when time.Time) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", gitDir, "commit-tree", tree, "-p", parent, "-m", message)
	date := when.Format(time.RFC3339)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME="+name,
		"GIT_AUTHOR_EMAIL="+email,
		"GIT_AUTHOR_DATE="+date,
		"GIT_COMMITTER_NAME="+name,
		"GIT_COMMITTER_EMAIL="+email,
		"GIT_COMMITTER_DATE="+date,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("webedit: commit-tree: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func gitOutput(ctx context.Context, gitDir, indexPath string, args ...string) (string, error) {
	cmdArgs := append([]string{"-C", gitDir}, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	if indexPath != "" {
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+indexPath)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func classifyUpdateRefError(err error) error {
	msg := err.Error()
	if strings.Contains(msg, "cannot lock ref") || strings.Contains(msg, "is at") || strings.Contains(msg, "but expected") {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return fmt.Errorf("webedit: update-ref: %w", err)
}

func enqueuePushProcess(ctx context.Context, pool *pgxpool.Pool, repoID, actorUserID int64, before, after, ref, requestID string) (int64, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("webedit: begin push event tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	event, err := workerdb.New().InsertPushEvent(ctx, tx, workerdb.InsertPushEventParams{
		RepoID:       repoID,
		BeforeSha:    before,
		AfterSha:     after,
		Ref:          ref,
		Protocol:     "web",
		PusherUserID: pgtype.Int8{Int64: actorUserID, Valid: actorUserID != 0},
		RequestID:    pgtype.Text{String: requestID, Valid: requestID != ""},
	})
	if err != nil {
		return 0, fmt.Errorf("webedit: insert push event: %w", err)
	}
	if _, err := worker.Enqueue(ctx, tx, worker.KindPushProcess, struct {
		PushEventID int64 `json:"push_event_id"`
	}{PushEventID: event.ID}, worker.EnqueueOptions{}); err != nil {
		return 0, err
	}
	_ = worker.Notify(ctx, tx) // Workers also poll; keep the commit path live if NOTIFY fails.
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("webedit: commit push event tx: %w", err)
	}
	committed = true
	return event.ID, nil
}

func resolveAuthor(ctx context.Context, pool *pgxpool.Pool, userID int64) (name, addr string, err error) {
	uq := usersdb.New()
	user, err := uq.GetUserByID(ctx, pool, userID)
	if err != nil {
		return "", "", fmt.Errorf("webedit: load user: %w", err)
	}
	if !user.PrimaryEmailID.Valid {
		return "", "", ErrNoVerifiedEmail
	}
	em, err := uq.GetUserEmailByID(ctx, pool, user.PrimaryEmailID.Int64)
	if err != nil {
		return "", "", fmt.Errorf("webedit: load primary email: %w", err)
	}
	if !em.Verified {
		return "", "", ErrNoVerifiedEmail
	}
	display := strings.TrimSpace(user.DisplayName)
	if display == "" {
		display = user.Username
	}
	return display, string(em.Email), nil
}

func validBranchName(branch string) bool {
	if branch == "" || len(branch) == 40 && isHex(branch) {
		return false
	}
	if strings.HasPrefix(branch, "-") || strings.HasPrefix(branch, "/") || strings.HasSuffix(branch, "/") {
		return false
	}
	if strings.Contains(branch, "\\") || strings.Contains(branch, "..") || strings.Contains(branch, "//") {
		return false
	}
	if strings.HasSuffix(branch, ".lock") || strings.Contains(branch, "@{") {
		return false
	}
	for _, c := range branch {
		if c < 0x20 || c == 0x7f || strings.ContainsRune(" ~^:?*[", c) {
			return false
		}
	}
	return true
}

func isHex(s string) bool {
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

func parentPath(p string) string {
	idx := strings.LastIndex(p, "/")
	if idx < 0 {
		return ""
	}
	return p[:idx]
}
