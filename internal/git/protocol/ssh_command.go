// SPDX-License-Identifier: AGPL-3.0-or-later

package protocol

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ParsedSSHCommand is the validated shape of `SSH_ORIGINAL_COMMAND`. The
// dispatcher resolves Owner+Repo against the DB and feeds Service to
// the same exec helper used by the HTTP transport.
type ParsedSSHCommand struct {
	Service Service // UploadPack or ReceivePack
	Owner   string  // already lowercase, validated via the same shape rules as RepoFS
	Repo    string  // already lowercase, validated via repo-name rules
}

// sshCmdRE captures the only acceptable input shape:
//
//	git-upload-pack 'owner/repo(.git)?'
//	git-upload-pack owner/repo(.git)?
//	git-receive-pack ...
//
// Single quotes around the path are optional but MUST MATCH — `a/b'`
// (one trailing quote, no leading) is rejected. The path group has
// two alternatives: a single-quoted captured form OR an unquoted form
// that may contain neither quotes nor whitespace. Anything else —
// extra args, embedded shell metacharacters, escaped quotes — is a
// hard reject. We never invoke a shell so we don't need escapes.
var sshCmdRE = regexp.MustCompile(`^(git-upload-pack|git-receive-pack)\s+(?:'([^']+)'|([^'\s]+))$`)

// ErrUnknownSSHCommand is returned when SSH_ORIGINAL_COMMAND doesn't
// match the strict whitelist. Callers should surface a friendly
// "shithub does not allow shell access" message and exit non-zero.
var ErrUnknownSSHCommand = errors.New("ssh: unsupported command")

// ErrInvalidSSHPath is returned when the parsed path fails owner/repo
// validation. This is the path-traversal defense.
var ErrInvalidSSHPath = errors.New("ssh: invalid repo path")

// ParseSSHCommand strict-parses SSH_ORIGINAL_COMMAND into Service +
// Owner + Repo. Returns ErrUnknownSSHCommand for anything that isn't a
// `git-{upload,receive}-pack <path>` invocation, and ErrInvalidSSHPath
// when the path fails the storage whitelist.
//
// Caller is expected to have already trimmed surrounding whitespace if
// any; we additionally enforce no leading/trailing whitespace ourselves.
func ParseSSHCommand(cmd string) (ParsedSSHCommand, error) {
	if cmd != strings.TrimSpace(cmd) {
		return ParsedSSHCommand{}, ErrUnknownSSHCommand
	}
	m := sshCmdRE.FindStringSubmatch(cmd)
	if m == nil {
		return ParsedSSHCommand{}, ErrUnknownSSHCommand
	}
	// The path group has two alternatives — quoted (m[2]) and unquoted
	// (m[3]). Whichever fired is the path we use.
	svc, path := Service(m[1]), m[2]
	if path == "" {
		path = m[3]
	}

	// Strip a single trailing `.git` if present.
	path = strings.TrimSuffix(path, ".git")

	owner, repo, ok := strings.Cut(path, "/")
	if !ok {
		return ParsedSSHCommand{}, fmt.Errorf("%w: missing owner/repo split", ErrInvalidSSHPath)
	}
	if strings.Contains(repo, "/") {
		return ParsedSSHCommand{}, fmt.Errorf("%w: extra path segments", ErrInvalidSSHPath)
	}
	owner = strings.ToLower(owner)
	repo = strings.ToLower(repo)
	if err := validateOwner(owner); err != nil {
		return ParsedSSHCommand{}, fmt.Errorf("%w: %v", ErrInvalidSSHPath, err)
	}
	if err := validateRepo(repo); err != nil {
		return ParsedSSHCommand{}, fmt.Errorf("%w: %v", ErrInvalidSSHPath, err)
	}
	return ParsedSSHCommand{Service: svc, Owner: owner, Repo: repo}, nil
}

// ownerRE / repoRE mirror storage's name validation. We don't import
// the storage package directly to avoid a runtime dependency in the
// SSH-shell hot path (every SSH connection runs through ParseSSHCommand
// and the storage package pulls in extra init).
var (
	ownerRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?$`)
	repoRE  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,98}[a-z0-9_])?$`)
)

func validateOwner(s string) error {
	if !ownerRE.MatchString(s) {
		return fmt.Errorf("owner shape")
	}
	if strings.Contains(s, "..") {
		return fmt.Errorf("dot-dot")
	}
	return nil
}

func validateRepo(s string) error {
	if !repoRE.MatchString(s) {
		return fmt.Errorf("repo shape")
	}
	if strings.Contains(s, "..") {
		return fmt.Errorf("dot-dot")
	}
	if strings.HasPrefix(s, ".") {
		return fmt.Errorf("starts with dot")
	}
	return nil
}
