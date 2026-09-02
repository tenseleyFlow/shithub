// SPDX-License-Identifier: AGPL-3.0-or-later

package metrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func writeHeartbeat(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestBackupHeartbeatCollector(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			name:  "no heartbeat files",
			files: nil,
			want:  "",
		},
		{
			name: "both jobs healthy",
			files: map[string]string{
				"backup-last-success":      "1767225420\n",
				"spaces-sync-last-success": "1767229020\n",
			},
			want: `
shithub_backup_last_success_seconds{job="daily"} 1.76722542e+09
shithub_backup_last_success_seconds{job="spaces-sync"} 1.76722902e+09
`,
		},
		{
			name: "only the daily job has ever succeeded",
			files: map[string]string{
				"backup-last-success": "1767225420",
			},
			want: `
shithub_backup_last_success_seconds{job="daily"} 1.76722542e+09
`,
		},
		{
			// A truncated or half-written file must not read as a
			// 1970 success — the series stays absent.
			name: "garbage content is absent, not zero",
			files: map[string]string{
				"backup-last-success":      "not-a-timestamp",
				"spaces-sync-last-success": "",
			},
			want: "",
		},
		{
			name: "zero timestamp is absent",
			files: map[string]string{
				"backup-last-success": "0",
			},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range tc.files {
				writeHeartbeat(t, dir, name, content)
			}

			reg := prometheus.NewPedanticRegistry()
			reg.MustRegister(NewBackupHeartbeatCollector(dir))

			header := "# HELP shithub_backup_last_success_seconds " +
				"Unix timestamp of the last fully successful run of each backup job, " +
				"from its heartbeat file. The series is absent when the job has never " +
				"succeeded on this host, so alert on absent() as well as on age.\n" +
				"# TYPE shithub_backup_last_success_seconds gauge\n"
			expected := ""
			if tc.want != "" {
				expected = header + strings.TrimPrefix(tc.want, "\n")
			}

			if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
				"shithub_backup_last_success_seconds"); err != nil {
				t.Errorf("gathered metrics: %v", err)
			}
		})
	}
}

// The default collector is registered at init against the production
// path, which does not exist in CI. It must degrade to zero series
// rather than erroring the whole /metrics scrape.
func TestBackupHeartbeatCollectorMissingDirIsSilent(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(NewBackupHeartbeatCollector(filepath.Join(t.TempDir(), "absent")))

	got, err := testutil.GatherAndCount(reg, "shithub_backup_last_success_seconds")
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if got != 0 {
		t.Errorf("series count = %d, want 0", got)
	}
}
