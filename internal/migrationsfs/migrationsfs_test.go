// SPDX-License-Identifier: AGPL-3.0-or-later

package migrationsfs

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"testing"
)

func TestMigrationVersionsAreUnique(t *testing.T) {
	t.Parallel()

	entries, err := fs.ReadDir(FS(), ".")
	if err != nil {
		t.Fatalf("ReadDir migrations: %v", err)
	}

	byVersion := make(map[string][]string)
	for _, entry := range entries {
		if entry.IsDir() || !isGooseMigrationName(entry.Name()) {
			continue
		}
		version := entry.Name()[:4]
		byVersion[version] = append(byVersion[version], entry.Name())
	}

	duplicates := make([]string, 0)
	for version, names := range byVersion {
		if len(names) <= 1 {
			continue
		}
		sort.Strings(names)
		duplicates = append(duplicates, fmt.Sprintf("%s: %s", version, strings.Join(names, ", ")))
	}
	sort.Strings(duplicates)
	if len(duplicates) > 0 {
		t.Fatalf("duplicate goose migration versions:\n%s", strings.Join(duplicates, "\n"))
	}
}

func TestFunctionMigrationsUseGooseStatementBlocks(t *testing.T) {
	t.Parallel()

	entries, err := fs.ReadDir(FS(), ".")
	if err != nil {
		t.Fatalf("ReadDir migrations: %v", err)
	}

	var problems []string
	for _, entry := range entries {
		if entry.IsDir() || !isGooseMigrationName(entry.Name()) {
			continue
		}
		body, err := fs.ReadFile(FS(), entry.Name())
		if err != nil {
			t.Fatalf("ReadFile %s: %v", entry.Name(), err)
		}

		inStatementBlock := false
		for lineNo, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			switch trimmed {
			case "-- +goose StatementBegin":
				inStatementBlock = true
			case "-- +goose StatementEnd":
				inStatementBlock = false
			}

			upper := strings.ToUpper(trimmed)
			createsFunction := strings.HasPrefix(upper, "CREATE FUNCTION ") ||
				strings.HasPrefix(upper, "CREATE OR REPLACE FUNCTION ") ||
				strings.HasPrefix(upper, "DO $$")
			if createsFunction && !inStatementBlock {
				problems = append(problems, fmt.Sprintf("%s:%d creates a multi-statement SQL block without -- +goose StatementBegin", entry.Name(), lineNo+1))
			}
		}
	}

	if len(problems) > 0 {
		t.Fatalf("goose SQL function blocks must be wrapped:\n%s", strings.Join(problems, "\n"))
	}
}

func isGooseMigrationName(name string) bool {
	if len(name) < len("0000_.sql") || !strings.HasSuffix(name, ".sql") || name[4] != '_' {
		return false
	}
	for _, r := range name[:4] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
