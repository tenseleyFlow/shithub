#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Provision a small DigitalOcean-backed shithub Actions runner pool.
#
# This intentionally creates cattle runner hosts only. It does not register
# runner tokens and does not write any shithub production secrets. Registration
# tokens are generated with shithubd and distributed by Ansible after droplets
# exist.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

POOL_NAME="${POOL_NAME:-shared-linux}"
PROJECT_NAME="${PROJECT_NAME:-shithub-prod}"
REGION="${REGION:-sfo3}"
SIZE="${SIZE:-s-2vcpu-4gb}"
IMAGE="${IMAGE:-ubuntu-24-04-x64}"
COUNT="${COUNT:-1}"
SSH_KEY_NAME="${SSH_KEY_NAME:-}"
SSH_ALLOWED_CIDRS="${SSH_ALLOWED_CIDRS:-}"
VPC_UUID="${VPC_UUID:-}"
RESOURCE_TAG="${RESOURCE_TAG:-shithub-actions-runner}"
POOL_TAG="${POOL_TAG:-}"
FIREWALL_NAME="${FIREWALL_NAME:-}"
USER_DATA_FILE="${USER_DATA_FILE:-$SCRIPT_DIR/actions-runner-cloud-init.yaml}"
DRY_RUN=0

usage() {
	cat <<'USAGE'
Usage:
  deploy/doctl/provision-actions-runner-pool.sh [flags]

Flags:
  --pool-name NAME          Pool slug used in droplet names (default: shared-linux)
  --project-name NAME       DigitalOcean project name (default: shithub-prod)
  --region SLUG             Droplet region (default: sfo3)
  --size SLUG               Droplet size (default: s-2vcpu-4gb)
  --image SLUG              Droplet image (default: ubuntu-24-04-x64)
  --count N                 Desired droplet count for this pool (default: 1)
  --ssh-key-name NAME       DigitalOcean SSH key name to install for root
  --ssh-allowed-cidrs LIST  Comma-separated CIDRs allowed to SSH to runners
  --vpc-uuid UUID           Optional VPC UUID for the droplets
  --resource-tag TAG        Shared tag for all runner droplets (default: shithub-actions-runner)
  --pool-tag TAG            Extra pool tag (default: shithub-actions-<pool-name>)
  --firewall-name NAME      Cloud firewall name (default: shithub-actions-runners-<pool-name>)
  --user-data-file PATH     Cloud-init file with no secrets
  --dry-run                 Validate inputs and print the plan without creating resources
  -h, --help                Show this help

Environment variables with the same uppercase names are also honored.

Example:
  SSH_KEY_NAME=macbook-pro \
  SSH_ALLOWED_CIDRS=203.0.113.4/32 \
  ./deploy/doctl/provision-actions-runner-pool.sh --dry-run
USAGE
}

fatal() {
	echo "fatal: $*" >&2
	exit 2
}

log() {
	echo "$*" >&2
}

trim() {
	local s="$1"
	s="${s#"${s%%[![:space:]]*}"}"
	s="${s%"${s##*[![:space:]]}"}"
	printf '%s' "$s"
}

require_tool() {
	command -v "$1" >/dev/null 2>&1 || fatal "$1 not on PATH"
}

while [[ $# -gt 0 ]]; do
	case "$1" in
	--pool-name)
		POOL_NAME="${2:?missing value for --pool-name}"
		shift 2
		;;
	--project-name)
		PROJECT_NAME="${2:?missing value for --project-name}"
		shift 2
		;;
	--region)
		REGION="${2:?missing value for --region}"
		shift 2
		;;
	--size)
		SIZE="${2:?missing value for --size}"
		shift 2
		;;
	--image)
		IMAGE="${2:?missing value for --image}"
		shift 2
		;;
	--count)
		COUNT="${2:?missing value for --count}"
		shift 2
		;;
	--ssh-key-name)
		SSH_KEY_NAME="${2:?missing value for --ssh-key-name}"
		shift 2
		;;
	--ssh-allowed-cidrs)
		SSH_ALLOWED_CIDRS="${2:?missing value for --ssh-allowed-cidrs}"
		shift 2
		;;
	--vpc-uuid)
		VPC_UUID="${2:?missing value for --vpc-uuid}"
		shift 2
		;;
	--resource-tag)
		RESOURCE_TAG="${2:?missing value for --resource-tag}"
		shift 2
		;;
	--pool-tag)
		POOL_TAG="${2:?missing value for --pool-tag}"
		shift 2
		;;
	--firewall-name)
		FIREWALL_NAME="${2:?missing value for --firewall-name}"
		shift 2
		;;
	--user-data-file)
		USER_DATA_FILE="${2:?missing value for --user-data-file}"
		shift 2
		;;
	--dry-run)
		DRY_RUN=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		fatal "unknown flag: $1"
		;;
	esac
done

[[ "$POOL_NAME" =~ ^[a-z0-9][a-z0-9-]*$ ]] || fatal "pool name must be a lowercase slug"
[[ "$RESOURCE_TAG" =~ ^[A-Za-z0-9:_.-]+$ ]] || fatal "resource tag contains unsupported characters"
[[ "$COUNT" =~ ^[0-9]+$ ]] || fatal "count must be a positive integer"
(( COUNT > 0 )) || fatal "count must be greater than zero"
[[ -n "$REGION" ]] || fatal "region is required"
[[ -n "$SIZE" ]] || fatal "size is required"
[[ -n "$IMAGE" ]] || fatal "image is required"
[[ -n "$SSH_KEY_NAME" ]] || fatal "set --ssh-key-name or SSH_KEY_NAME"
[[ -n "$SSH_ALLOWED_CIDRS" ]] || fatal "set --ssh-allowed-cidrs or SSH_ALLOWED_CIDRS"
[[ -r "$USER_DATA_FILE" ]] || fatal "user-data file not readable: $USER_DATA_FILE"

if [[ -z "$POOL_TAG" ]]; then
	POOL_TAG="shithub-actions-$POOL_NAME"
fi
if [[ -z "$FIREWALL_NAME" ]]; then
	FIREWALL_NAME="shithub-actions-runners-$POOL_NAME"
fi

SSH_RULES=()
IFS=',' read -r -a CIDR_PARTS <<<"$SSH_ALLOWED_CIDRS"
for raw in "${CIDR_PARTS[@]}"; do
	cidr="$(trim "$raw")"
	[[ -n "$cidr" ]] || continue
	case "$cidr" in
	0.0.0.0/0 | ::/0 | 0/0)
		fatal "refusing public SSH CIDR $cidr; use your operator/VPN IP range"
		;;
	esac
	[[ "$cidr" == */* ]] || fatal "SSH CIDR must include a prefix length: $cidr"
	SSH_RULES+=("protocol:tcp,ports:22,address:$cidr")
done
(( ${#SSH_RULES[@]} > 0 )) || fatal "at least one non-public SSH CIDR is required"
SSH_INBOUND_RULES="${SSH_RULES[*]}"
# DigitalOcean firewall rules accept explicit TCP/UDP port ranges here, not
# the human shorthand "all". Keep this broad at the cloud firewall layer; the
# runner host's ipset firewall enforces the DNS allowlist for job containers.
OUTBOUND_RULES="protocol:tcp,ports:1-65535,address:0.0.0.0/0 protocol:udp,ports:1-65535,address:0.0.0.0/0 protocol:icmp,address:0.0.0.0/0"

require_tool doctl
require_tool jq

if ! doctl auth list >/dev/null 2>&1 || ! doctl account get >/dev/null 2>&1; then
	fatal "doctl is not authenticated; run 'doctl auth init'"
fi

SSH_KEY_ID="$(doctl compute ssh-key list --output json | jq -r --arg name "$SSH_KEY_NAME" 'first(.[] | select(.name == $name) | .id) // ""')"
[[ -n "$SSH_KEY_ID" ]] || fatal "no DigitalOcean SSH key named $SSH_KEY_NAME"

PROJECT_ID="$(doctl projects list --output json | jq -r --arg name "$PROJECT_NAME" 'first(.[] | select(.name == $name) | .id) // ""')"
if [[ -z "$PROJECT_ID" ]]; then
	if (( DRY_RUN )); then
		PROJECT_ID="dry-run-project-id"
		log "would create project $PROJECT_NAME"
	else
		log "creating project $PROJECT_NAME"
		PROJECT_ID="$(doctl projects create \
			--name "$PROJECT_NAME" \
			--purpose "Service or API" \
			--environment Production \
			--description "shithub Actions runner pool" \
			--no-header --format ID)"
	fi
else
	log "project $PROJECT_NAME exists ($PROJECT_ID)"
fi

ensure_tag() {
	local tag="$1"
	if doctl compute tag list --output json | jq -e --arg name "$tag" 'any(.[]; .name == $name)' >/dev/null; then
		log "tag $tag exists"
		return
	fi
	if (( DRY_RUN )); then
		log "would create tag $tag"
		return
	fi
	log "creating tag $tag"
	doctl compute tag create "$tag" >/dev/null
}

ensure_tag "$RESOURCE_TAG"
ensure_tag "$POOL_TAG"

FIREWALL_ID="$(doctl compute firewall list --no-header --format ID,Name | awk -v n="$FIREWALL_NAME" '$2==n {print $1; exit}')"
if [[ -z "$FIREWALL_ID" ]]; then
	if (( DRY_RUN )); then
		FIREWALL_ID="dry-run-firewall-id"
		log "would create firewall $FIREWALL_NAME for tag $RESOURCE_TAG"
	else
		log "creating firewall $FIREWALL_NAME for tag $RESOURCE_TAG"
		FIREWALL_ID="$(doctl compute firewall create \
			--name "$FIREWALL_NAME" \
			--tag-names "$RESOURCE_TAG" \
			--inbound-rules "$SSH_INBOUND_RULES" \
			--outbound-rules "$OUTBOUND_RULES" \
			--no-header --format ID)"
	fi
else
	log "firewall $FIREWALL_NAME exists ($FIREWALL_ID); leaving rules unchanged"
fi

NAME_PREFIX="shithub-runner-$POOL_NAME-"

droplet_id_by_name() {
	local name="$1"
	doctl compute droplet list --no-header --format ID,Name | awk -v n="$name" '$2==n {print $1; exit}'
}

created_or_reused=()
for i in $(seq 1 "$COUNT"); do
	name="$NAME_PREFIX$i"
	existing="$(droplet_id_by_name "$name")"
	if [[ -n "$existing" ]]; then
		log "droplet $name exists ($existing); skipping"
		created_or_reused+=("$existing:$name:existing")
		continue
	fi

	if (( DRY_RUN )); then
		log "would create droplet $name ($REGION, $SIZE, $IMAGE)"
		created_or_reused+=("dry-run-$i:$name:planned")
		continue
	fi

	cmd=(doctl compute droplet create "$name"
		--image "$IMAGE"
		--region "$REGION"
		--size "$SIZE"
		--ssh-keys "$SSH_KEY_ID"
		--enable-monitoring
		--tag-names "$RESOURCE_TAG,$POOL_TAG"
		--user-data-file "$USER_DATA_FILE"
		--project-id "$PROJECT_ID"
		--wait
		--no-header
		--format ID)
	if [[ -n "$VPC_UUID" ]]; then
		cmd+=(--vpc-uuid "$VPC_UUID")
	fi

	log "creating droplet $name ($REGION, $SIZE, $IMAGE)"
	id="$("${cmd[@]}")"
	created_or_reused+=("$id:$name:created")
done

if (( ! DRY_RUN )); then
	resource_args=()
	for entry in "${created_or_reused[@]}"; do
		id="${entry%%:*}"
		resource_args+=(--resource "do:droplet:$id")
	done
	if (( ${#resource_args[@]} > 0 )); then
		log "assigning runner droplets to project $PROJECT_NAME"
		doctl projects resources assign "$PROJECT_ID" "${resource_args[@]}" >/dev/null
	fi
fi

if (( DRY_RUN )); then
	droplets_json="$(
		printf '%s\n' "${created_or_reused[@]}" |
			jq -Rn '[inputs | split(":") | {
				id: .[0],
				name: .[1],
				status: .[2],
				public_ipv4: null,
				private_ipv4: null
			}]'
	)"
else
	droplets_json="$(doctl compute droplet list --tag-name "$RESOURCE_TAG" --output json |
		jq --arg prefix "$NAME_PREFIX" '[.[] | select(.name | startswith($prefix)) | {
			id: (.id | tostring),
			name: .name,
			status: .status,
			public_ipv4: ((.networks.v4 // []) | map(select(.type == "public")) | first | .ip_address // null),
			private_ipv4: ((.networks.v4 // []) | map(select(.type == "private")) | first | .ip_address // null)
		}] | sort_by(.name)')"
fi

jq -n \
	--arg pool_name "$POOL_NAME" \
	--arg project_name "$PROJECT_NAME" \
	--arg project_id "$PROJECT_ID" \
	--arg region "$REGION" \
	--arg size "$SIZE" \
	--arg image "$IMAGE" \
	--arg resource_tag "$RESOURCE_TAG" \
	--arg pool_tag "$POOL_TAG" \
	--arg firewall_name "$FIREWALL_NAME" \
	--arg firewall_id "$FIREWALL_ID" \
	--argjson droplets "$droplets_json" \
	'{
		pool_name: $pool_name,
		project: {name: $project_name, id: $project_id},
		region: $region,
		size: $size,
		image: $image,
		tags: [$resource_tag, $pool_tag],
		firewall: {name: $firewall_name, id: $firewall_id},
		droplets: $droplets
	}'
