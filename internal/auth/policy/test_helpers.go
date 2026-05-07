// SPDX-License-Identifier: AGPL-3.0-or-later

package policy

import "context"

// PrimeCacheForTest seeds the request cache with a (user, repo) → role
// entry so tests can drive Can() without a live DB. Only call from
// _test.go files; the export is intentionally export-tagged-as-test in
// its name so production callers don't reach for it.
func PrimeCacheForTest(ctx context.Context, userID, repoID int64, role Role) {
	cachePut(cacheFromContext(ctx), cacheKey{actorUserID: userID, repoID: repoID}, role)
}
