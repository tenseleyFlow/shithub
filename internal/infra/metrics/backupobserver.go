// SPDX-License-Identifier: AGPL-3.0-or-later

package metrics

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// Backup jobs run from root crontab on the app box, not from a systemd
// timer, and there is no Alertmanager to catch a non-zero exit (see
// deploy/monitoring/README.md). Each job writes a heartbeat file
// containing epoch seconds *only* when it fully succeeds; this
// collector turns those files into a gauge so the one signal that does
// leave the box — Alloy's remote_write to Grafana Cloud — carries
// backup freshness.
//
// Read at scrape time rather than cached: the files change a few times
// a day and a scrape is two stat+read calls on a tmpfs-warm path.
const defaultBackupHeartbeatDir = "/var/lib/shithub"

// job label -> file name under the heartbeat dir. Keep in sync with
// deploy/postgres/backup-daily.sh and deploy/spaces/sync-cross-region.sh.
var backupHeartbeatFiles = map[string]string{
	"daily":       "backup-last-success",
	"spaces-sync": "spaces-sync-last-success",
}

var backupLastSuccessDesc = prometheus.NewDesc(
	"shithub_backup_last_success_seconds",
	"Unix timestamp of the last fully successful run of each backup job, from its heartbeat file. The series is absent when the job has never succeeded on this host, so alert on absent() as well as on age.",
	[]string{"job"},
	nil,
)

// BackupHeartbeatCollector reports the mtime-independent timestamp
// each backup job records on success.
type BackupHeartbeatCollector struct {
	dir string
}

// NewBackupHeartbeatCollector reads heartbeat files from dir. An empty
// dir means the production default.
func NewBackupHeartbeatCollector(dir string) *BackupHeartbeatCollector {
	if dir == "" {
		dir = defaultBackupHeartbeatDir
	}
	return &BackupHeartbeatCollector{dir: dir}
}

func (c *BackupHeartbeatCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- backupLastSuccessDesc
}

func (c *BackupHeartbeatCollector) Collect(ch chan<- prometheus.Metric) {
	for job, name := range backupHeartbeatFiles {
		ts, ok := readBackupHeartbeat(filepath.Join(c.dir, name))
		if !ok {
			// No file, unreadable, or garbage. Emitting 0 here would
			// read as "succeeded in 1970" and fire an age alert
			// identically to a real overdue backup, but it would also
			// mask the never-ran case; absent is the honest answer.
			continue
		}
		ch <- prometheus.MustNewConstMetric(
			backupLastSuccessDesc, prometheus.GaugeValue, ts, job,
		)
	}
}

// readBackupHeartbeat parses the epoch-seconds line a backup script
// writes. Anything unexpected is reported as absent rather than as a
// zero sample.
func readBackupHeartbeat(path string) (float64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return float64(n), true
}

func init() {
	Registry.MustRegister(NewBackupHeartbeatCollector(""))
}
