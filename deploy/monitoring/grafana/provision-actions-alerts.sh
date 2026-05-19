#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Idempotently provision shithub Actions alert rules into Grafana Cloud.
#
# This is intentionally separate from deploy/monitoring/prometheus/rules.yml:
# production pushes metrics to Grafana Cloud with Alloy remote_write, so there
# is no local Prometheus process loading that rules file.
#
# Required for --apply or datasource discovery:
#   GRAFANA_URL    https://<stack>.grafana.net
#   GRAFANA_TOKEN  service-account token with alerting + folder read/write
#
# Optional:
#   GRAFANA_PROM_DATASOURCE_UID       Prometheus/Mimir datasource UID
#   GRAFANA_PROM_DATASOURCE_NAME      exact datasource name to discover
#   GRAFANA_ORG_ID                    default: 1
#   GRAFANA_FOLDER_UID                default: shithub-ops
#   GRAFANA_FOLDER_TITLE              default: shithub Operations
#   GRAFANA_RULE_GROUP                default: shithubd-actions
#
# Usage:
#   GRAFANA_URL=https://shithub.grafana.net \
#   GRAFANA_TOKEN=glsa_... \
#   ./deploy/monitoring/grafana/provision-actions-alerts.sh --dry-run
#
#   GRAFANA_URL=https://shithub.grafana.net \
#   GRAFANA_TOKEN=glsa_... \
#   ./deploy/monitoring/grafana/provision-actions-alerts.sh --apply

set -euo pipefail

SCRIPT_NAME="$(basename "$0")"
DRY_RUN=1

usage() {
	cat >&2 <<EOF
usage: $SCRIPT_NAME [--dry-run|--apply]

Environment:
  GRAFANA_URL                       required for --apply/discovery, e.g. https://shithub.grafana.net
  GRAFANA_TOKEN                     required for --apply/discovery service-account token
  GRAFANA_PROM_DATASOURCE_UID       optional Prometheus datasource UID
  GRAFANA_PROM_DATASOURCE_NAME      optional exact datasource name
  GRAFANA_ORG_ID                    optional, default 1
  GRAFANA_FOLDER_UID                optional, default shithub-ops
  GRAFANA_FOLDER_TITLE              optional, default "shithub Operations"
  GRAFANA_RULE_GROUP                optional, default shithubd-actions
EOF
}

while (($#)); do
	case "$1" in
		--dry-run)
			DRY_RUN=1
			;;
		--apply)
			DRY_RUN=0
			;;
		-h|--help)
			usage
			exit 0
			;;
		*)
			usage
			exit 2
			;;
	esac
	shift
done

require_tool() {
	if ! command -v "$1" >/dev/null 2>&1; then
		echo "fatal: $1 is required" >&2
		exit 2
	fi
}

require_tool curl
require_tool jq

GRAFANA_URL="${GRAFANA_URL:-}"
GRAFANA_TOKEN="${GRAFANA_TOKEN:-}"
GRAFANA_PROM_DATASOURCE_UID="${GRAFANA_PROM_DATASOURCE_UID:-}"
GRAFANA_PROM_DATASOURCE_NAME="${GRAFANA_PROM_DATASOURCE_NAME:-}"
GRAFANA_ORG_ID="${GRAFANA_ORG_ID:-1}"
GRAFANA_FOLDER_UID="${GRAFANA_FOLDER_UID:-shithub-ops}"
GRAFANA_FOLDER_TITLE="${GRAFANA_FOLDER_TITLE:-shithub Operations}"
GRAFANA_RULE_GROUP="${GRAFANA_RULE_GROUP:-shithubd-actions}"

if [[ "$DRY_RUN" == "0" && ( -z "$GRAFANA_URL" || -z "$GRAFANA_TOKEN" ) ]]; then
	usage
	exit 2
fi

GRAFANA_URL="${GRAFANA_URL%/}"

api() {
	local method="$1" path="$2" body="${3:-}"
	if [[ -n "$body" ]]; then
		curl -sS \
			-X "$method" \
			-H "Authorization: Bearer $GRAFANA_TOKEN" \
			-H "Accept: application/json" \
			-H "Content-Type: application/json" \
			-H "X-Disable-Provenance: true" \
			-d "$body" \
			"$GRAFANA_URL$path"
	else
		curl -sS \
			-X "$method" \
			-H "Authorization: Bearer $GRAFANA_TOKEN" \
			-H "Accept: application/json" \
			-H "X-Disable-Provenance: true" \
			"$GRAFANA_URL$path"
	fi
}

api_status() {
	local method="$1" path="$2" output="$3" body="${4:-}"
	if [[ -n "$body" ]]; then
		curl -sS \
			-o "$output" \
			-w '%{http_code}' \
			-X "$method" \
			-H "Authorization: Bearer $GRAFANA_TOKEN" \
			-H "Accept: application/json" \
			-H "Content-Type: application/json" \
			-H "X-Disable-Provenance: true" \
			-d "$body" \
			"$GRAFANA_URL$path"
	else
		curl -sS \
			-o "$output" \
			-w '%{http_code}' \
			-X "$method" \
			-H "Authorization: Bearer $GRAFANA_TOKEN" \
			-H "Accept: application/json" \
			-H "X-Disable-Provenance: true" \
			"$GRAFANA_URL$path"
	fi
}

discover_prometheus_datasource_uid() {
	if [[ -n "$GRAFANA_PROM_DATASOURCE_UID" ]]; then
		printf '%s\n' "$GRAFANA_PROM_DATASOURCE_UID"
		return
	fi
	if [[ -z "$GRAFANA_URL" || -z "$GRAFANA_TOKEN" ]]; then
		echo "fatal: set GRAFANA_PROM_DATASOURCE_UID for offline dry-runs, or set GRAFANA_URL and GRAFANA_TOKEN for discovery" >&2
		exit 2
	fi

	local datasources uid_count uid
	datasources="$(api GET /api/datasources)"
	if [[ -n "$GRAFANA_PROM_DATASOURCE_NAME" ]]; then
		uid="$(jq -r --arg name "$GRAFANA_PROM_DATASOURCE_NAME" '
			[.[] | select(.type == "prometheus" and .name == $name) | .uid]
			| if length == 1 then .[0] else empty end
		' <<<"$datasources")"
		if [[ -n "$uid" ]]; then
			printf '%s\n' "$uid"
			return
		fi
		echo "fatal: no unique Prometheus datasource named $GRAFANA_PROM_DATASOURCE_NAME" >&2
		exit 2
	fi

	uid_count="$(jq '[.[] | select(.type == "prometheus")] | length' <<<"$datasources")"
	if [[ "$uid_count" == "1" ]]; then
		jq -r '[.[] | select(.type == "prometheus") | .uid][0]' <<<"$datasources"
		return
	fi

	echo "fatal: found $uid_count Prometheus datasources; set GRAFANA_PROM_DATASOURCE_UID or GRAFANA_PROM_DATASOURCE_NAME" >&2
	jq -r '.[] | select(.type == "prometheus") | "  - \(.name) uid=\(.uid)"' <<<"$datasources" >&2
	exit 2
}

ensure_folder() {
	local tmp status body
	if [[ "$DRY_RUN" == "1" ]]; then
		echo "would ensure folder: $GRAFANA_FOLDER_TITLE ($GRAFANA_FOLDER_UID)" >&2
		return
	fi

	tmp="$(mktemp)"
	status="$(api_status GET "/api/folders/$GRAFANA_FOLDER_UID" "$tmp")"
	case "$status" in
		200)
			echo "folder exists: $GRAFANA_FOLDER_TITLE ($GRAFANA_FOLDER_UID)" >&2
			rm -f "$tmp"
			return
			;;
		404)
			rm -f "$tmp"
			;;
		*)
			echo "fatal: Grafana folder lookup failed with HTTP $status" >&2
			cat "$tmp" >&2
			rm -f "$tmp"
			exit 1
			;;
	esac

	body="$(jq -n --arg uid "$GRAFANA_FOLDER_UID" --arg title "$GRAFANA_FOLDER_TITLE" \
		'{uid: $uid, title: $title}')"

	tmp="$(mktemp)"
	status="$(api_status POST /api/folders "$tmp" "$body")"
	if [[ "$status" != "200" && "$status" != "201" ]]; then
		echo "fatal: Grafana folder create failed with HTTP $status" >&2
		cat "$tmp" >&2
		rm -f "$tmp"
		exit 1
	fi
	echo "folder created: $GRAFANA_FOLDER_TITLE ($GRAFANA_FOLDER_UID)" >&2
	rm -f "$tmp"
}

build_runner_idle_payload() {
	local datasource_uid="$1"
	jq -n \
		--arg uid "shithub-actions-runner-idle-with-assigned-jobs" \
		--arg title "Actions runner idle with assigned jobs" \
		--arg group "$GRAFANA_RULE_GROUP" \
		--arg folder "$GRAFANA_FOLDER_UID" \
		--arg datasource "$datasource_uid" \
		--arg expr 'shithub_actions_runner_active_jobs{status="idle"} > 0 and on (runner, status) shithub_actions_runner_heartbeat_age_seconds{status="idle"} < 60' \
		--argjson org_id "$GRAFANA_ORG_ID" \
		'{
			uid: $uid,
			title: $title,
			ruleGroup: $group,
			folderUID: $folder,
			noDataState: "OK",
			execErrState: "Error",
			for: "5m",
			orgID: $org_id,
			condition: "B",
			annotations: {
				summary: "Actions runner {{ $labels.runner }} is idle with assigned running jobs",
				runbook: "runbooks/incidents.md#actions-runner-idle-with-assigned-jobs"
			},
			labels: {
				severity: "page",
				service: "actions"
			},
			data: [
				{
					refId: "A",
					queryType: "",
					relativeTimeRange: {from: 600, to: 0},
					datasourceUid: $datasource,
					model: {
						datasource: {type: "prometheus", uid: $datasource},
						expr: $expr,
						instant: true,
						intervalMs: 1000,
						maxDataPoints: 43200,
						refId: "A"
					}
				},
				{
					refId: "B",
					queryType: "",
					relativeTimeRange: {from: 0, to: 0},
					datasourceUid: "-100",
					model: {
						conditions: [
							{
								evaluator: {params: [0], type: "gt"},
								operator: {type: "and"},
								query: {params: ["A"]},
								reducer: {params: [], type: "last"},
								type: "query"
							}
						],
						datasource: {type: "__expr__", uid: "-100"},
						hide: false,
						intervalMs: 1000,
						maxDataPoints: 43200,
						refId: "B",
						type: "classic_conditions"
					}
				}
			]
		}'
}

upsert_alert_rule() {
	local payload="$1" uid="$2" tmp status method path
	if [[ "$DRY_RUN" == "1" ]]; then
		echo "would upsert alert rule: $uid" >&2
		jq . <<<"$payload"
		return
	fi

	tmp="$(mktemp)"
	status="$(api_status GET "/api/v1/provisioning/alert-rules/$uid" "$tmp")"
	case "$status" in
		200)
			method=PUT
			path="/api/v1/provisioning/alert-rules/$uid"
			;;
		404)
			method=POST
			path="/api/v1/provisioning/alert-rules"
			;;
		*)
			echo "fatal: Grafana alert lookup failed with HTTP $status" >&2
			cat "$tmp" >&2
			rm -f "$tmp"
			exit 1
			;;
	esac
	rm -f "$tmp"

	tmp="$(mktemp)"
	status="$(api_status "$method" "$path" "$tmp" "$payload")"
	if [[ "$status" != "200" && "$status" != "201" && "$status" != "202" ]]; then
		echo "fatal: Grafana alert upsert failed with HTTP $status" >&2
		cat "$tmp" >&2
		rm -f "$tmp"
		exit 1
	fi
	echo "alert provisioned: $uid" >&2
	rm -f "$tmp"
}

datasource_uid="$(discover_prometheus_datasource_uid)"
echo "prometheus datasource: $datasource_uid" >&2
ensure_folder
payload="$(build_runner_idle_payload "$datasource_uid")"
upsert_alert_rule "$payload" "shithub-actions-runner-idle-with-assigned-jobs"
