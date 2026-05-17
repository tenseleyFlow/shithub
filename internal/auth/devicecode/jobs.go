// SPDX-License-Identifier: AGPL-3.0-or-later

package devicecode

import "github.com/tenseleyFlow/shithub/internal/worker"

// KindDeviceAuthorizationSweep is the worker job kind that prunes
// terminal/expired device_authorizations rows past their 24h forensics
// window. The DELETE itself lives in the sqlc query
// DeleteExpiredDeviceAuthorizations (internal/users/queries/device_authorizations.sql);
// the handler that drives the query lives in internal/worker/jobs/devicecode.go;
// the operator's external scheduler (systemd timer) enqueues the kind
// on a daily beat. See `S55-remediation.md` finding #5.
const KindDeviceAuthorizationSweep worker.Kind = "devicecode:sweep"
