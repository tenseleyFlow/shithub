// SPDX-License-Identifier: AGPL-3.0-or-later

package storage

// Quota is the disk-usage budget for a user or org. Recorded in the DB
// under users.disk_quota_* and orgs.disk_quota_* (added when those tables
// exist). S04 wires the type only — enforcement lives in a future policy
// package called from the push pipeline (S14) and attachment uploads.
type Quota struct {
	Used  int64 // bytes currently used
	Limit int64 // bytes allowed (0 = unlimited)
}

// Available returns Limit - Used, clamped at zero. Returns -1 when the
// quota is unlimited.
func (q Quota) Available() int64 {
	if q.Limit == 0 {
		return -1
	}
	if q.Used >= q.Limit {
		return 0
	}
	return q.Limit - q.Used
}

// WouldExceed reports whether writing additional bytes n would push past
// the limit. Always false for an unlimited quota.
func (q Quota) WouldExceed(n int64) bool {
	if q.Limit == 0 {
		return false
	}
	return q.Used+n > q.Limit
}
