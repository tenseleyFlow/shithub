// SPDX-License-Identifier: AGPL-3.0-or-later

package cronworkflow

import "github.com/tenseleyFlow/shithub/internal/worker"

// KindCronWorkflowSweep is the worker job kind. Invoked on a beat by
// the operator's systemd timer (the same mechanism that drives
// webhook:purge_old). One job per tick; the handler claims up to
// SweepBatch due rows and re-enqueues itself when a batch fills.
const KindCronWorkflowSweep worker.Kind = "cron_workflow_dispatch:sweep"
