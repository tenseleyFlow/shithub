# shithub.sh — first-deploy setup guide

This is the operator's running-order for taking shithub.sh from
"Namecheap registration" to "live and serving signups." Walk it
top-to-bottom, one step at a time. Each step has a verify-it-
worked check. **Don't skip the verifications** — they're cheap
and catch the wrong thing before it compounds.

> **Time budget.** Total ~5–8 hours across 1–2 days, dominated
> by DNS / Postmark verification waits (you can step away during
> those).

> **Money budget.** ~$3.50/day from Step 4 onwards
> (~$105/mo all-in once Spaces buckets are provisioned). If you
> have to pause for a day, fine; if you need to pause for a week,
> destroy the droplets — the volume + Spaces + DNS persist.

## Decisions baked in

- Domain: **shithub.sh** (you registered this on Namecheap)
- Region: **NYC3** (DO Spaces parity, codebase default)
- DR region: **SFO3** (cross-region Spaces mirror)
- Email: **Postmark** (free tier covers v0.1.0)
- Mirror plan: **90-day GitHub mirror, then drop**
- Version: **v0.1.0** (pre-1.0; honest about WIP; tag will be cut later)
- On-call: **email-only alerts for week 1**, flip to phone after
  noise calibration

If any of these change, redo the steps that depend on them.

---

## Phase A — Accounts & DNS (do these first; they propagate while you wait)

### A1. Postmark account

1. Go to <https://account.postmarkapp.com/sign_up>.
2. Sign up with the email you'll use for ops
   (`ops@shithub.sh` is a good convention; you'll set up the
   inbox later).
3. After signup, Postmark drops you in a default Server.
   Rename it: top-left dropdown → **Manage Servers** → click the
   default → **Settings** → name it **shithub-prod**.
4. Confirm the Server has the **Transactional** message stream
   (default) — it does for new accounts.
5. **Don't grab the API token yet** — we generate it after
   verifying the domain.

**Verify:** the Servers list shows one server named
`shithub-prod`.

### A2. Verify the sender domain in Postmark

This is the slow step. Start it now; come back later.

1. In the Postmark dashboard: top-right **Sender Signatures** →
   **Domains** tab → **Add Domain**.
2. Enter **`shithub.sh`** (the bare apex — DKIM applies to all
   subdomains).
3. Postmark presents three DNS records to add:
   - **DKIM** — a `TXT` record at `<postmark-prefix>._domainkey`
   - **Return-Path** — a `CNAME` at `pm-bounces` (for VERP-style
     bounce handling)
   - **(optional) DMARC** — a `TXT` at `_dmarc` — Postmark will
     suggest one; we'll add it.
4. Leave that tab open; we add the records in Namecheap next.

### A3. Add Postmark DNS records in Namecheap

1. <https://ap.www.namecheap.com/Domains/DomainControlPanel/shithub.sh/advancedns>
2. **Advanced DNS** tab. Set **NameServers** to **Namecheap
   BasicDNS** if it isn't already (the default after a fresh
   registration).
3. **Add a New Record** for each of the three Postmark records.
   Match the exact `Host` and `Value` Postmark gave you. TTL =
   1 min during setup (we'll relax it later).
4. Also add an **SPF record** if there isn't one already:
   - Type: `TXT`
   - Host: `@`
   - Value: `v=spf1 include:spf.mtasv.net ~all`
   - (`spf.mtasv.net` is Postmark's relay; the `~all` is
     soft-fail.)
5. **DMARC record** (recommended; Postmark prompts you):
   - Type: `TXT`
   - Host: `_dmarc`
   - Value: `v=DMARC1; p=none; rua=mailto:dmarc-rua@shithub.sh; pct=100`
   - `p=none` means "report only, don't reject" — appropriate
     for week 1 while we tune. Tighten to `p=quarantine` later.

**Verify:** in Postmark's **Domains** tab, click **Verify**.
DKIM may take 5–30 min to propagate. Other records refresh on
their own. The verification turns green once all records are
seen. Move on; come back to confirm.

### A4. Grab the Postmark API token

You can do this immediately after creating the Server — domain
verification is independent of token issuance.

1. Postmark → **Servers** → **shithub-prod** → **API Tokens**
   tab.
2. The default Server token shown there is what we'll use.
3. **Copy it.** Keep in your password manager.

**About the From address.** Postmark has no "Sender From" field
on the Server — the From string lives in the body of each API
call. Once your domain is DKIM-verified (Phase A2), every
address `*@shithub.sh` is authorized to send; no per-address
"Sender Signature" needed. We pass the literal From string to
Postmark via the inventory in Phase D3:

```
email_from: shithub <noreply@shithub.sh>
```

The `noreply@` mailbox doesn't need to actually exist as an
inbox — replies to it bounce, which is the documented behavior
(`docs/public/user/notifications.md` notes that reply-by-email
isn't supported).

### A5. Set up DNS for the app + docs subdomains

Still in Namecheap **Advanced DNS** for shithub.sh. Add:

- `A` record, Host `@`, Value `<APP-DROPLET-IP>` — **placeholder
  for now**; we'll fill the real IP after creating the droplet
  in Phase B. **Pin TTL to 1 min** for now.
- `A` record, Host `www`, Value `<APP-DROPLET-IP>` — same.
- `CNAME` record, Host `docs`, Value `shithub-docs.nyc3.cdn.digitaloceanspaces.com.`
  (Spaces CDN — we'll create the bucket in Phase B; the CNAME
  resolves once the bucket exists.) The trailing dot matters.

Skip the records you don't have IPs for yet; they go in after
Phase B step B3.

### A6. Telegram bot for alerts (skipped — week-1 email only)

We're using email-only alerts for week 1. Email goes via
Postmark to your ops mailbox. When you flip to phone alerts:
follow `docs/internal/runbooks/incidents.md` and add a Telegram
bot — won't repeat that here.

---

## Phase B — DigitalOcean infrastructure

### B1. DO project + SSH key

These are two unrelated resources — the project is a workspace
grouping, the SSH key is an account-wide credential. You'll
attach the key to each droplet at create time (Phase B3).

**Project:**

1. <https://cloud.digitalocean.com> → **Projects** (left sidebar)
   → **New Project**.
2. Name: **shithub-prod**. Purpose: **Service or API**.
   Environment: **Production**.

**SSH key — easiest path: add it during droplet creation in B3.**

DO's standalone "SSH Keys" settings page keeps moving around the
UI, so the most reliable place to add a new key is the droplet
create form itself:

- In Phase B3 step 6 ("Authentication"), click **"+ New SSH Key"**
  on the form. Paste `~/.ssh/id_ed25519.pub` from your laptop,
  name it after the laptop ("macbook-pro"). The key gets saved
  to your account permanently AND attached to this droplet.
- For droplets #2–4, the key is already in the list — just tick
  its checkbox.

If you want to add the key in advance via the standalone page:
use the dashboard's top search bar — type **"SSH"** — and it'll
surface the current location. (Path varies; "Settings → Security
→ SSH Keys" used to work, but DO reorganises this page often.)

**Verify:** the project shows under Projects. The SSH key
verification happens at droplet-create time when you tick the
box.

### B2. Create Spaces buckets (do these BEFORE droplets — they need to exist for the docs CNAME to resolve)

1. Left sidebar → **Spaces Object Storage** → **Create Spaces
   Bucket**.
2. **Bucket #1 — primary backups + docs:**
   - Region: **NYC3**
   - Name: **shithub-prod** (this matches the inventory's
     `s3_bucket: shithub-prod`)
   - File listing: **Restrict** (private; presigned URLs only)
   - CDN: **Enable** (for the docs subdomain)
   - Project: **shithub-prod**
3. **Bucket #2 — DR mirror:**
   - Region: **SFO3**
   - Name: **shithub-prod-dr**
   - Same other settings.
4. **Bucket #3 — docs site:**
   - Region: **NYC3**
   - Name: **shithub-docs**
   - File listing: **Public** (it's the docs site; everyone
     reads)
   - CDN: **Enable**
   - Project: **shithub-prod**
5. **Generate Spaces access keys:** Account → API → **Spaces
   Keys** → **Generate New Key**. Name it `shithub-prod-app`.
   Copy the access key + secret — Postmark-style, the secret
   is shown once.

**Verify:** three buckets listed in Spaces, all in their
respective regions. Endpoint URLs follow the pattern
`<bucket>.<region>.digitaloceanspaces.com`.

### B3. Create the four droplets

Use the DO web UI for the first one to confirm the shape; then
duplicate.

1. Left sidebar → **Droplets** → **Create Droplet**.
2. **Region:** NYC3 → Datacenter NYC3.
3. **Image:** Marketplace? **No** — Distributions tab → Ubuntu
   24.04 (LTS) x64.
4. **Size:** Basic → Regular SSD → **2 vCPU / 4 GB RAM /
   80 GB SSD** ($24/mo).
5. **VPC Network:** default VPC for NYC3 (DO selects this
   automatically). All four droplets must be in the same VPC so
   they see each other on private IPs.
6. **Authentication:** SSH Key → check the key you added in B1.
7. **Hostname:** `shithub-app` (this is droplet #1).
8. **Tags:** `shithub`, `shithub-app`.
9. **Project:** shithub-prod.
10. **Backups:** off (we have our own backup pipeline).
11. **Monitoring:** **on** (DO's free agent — useful baseline
    metrics in their UI).
12. Create.

Repeat for droplets #2–#4 with these differences:

| # | Hostname           | Size                  | Tags                       |
|---|--------------------|----------------------|----------------------------|
| 2 | `shithub-db`       | s-2vcpu-4gb ($24/mo) | `shithub`, `shithub-db`    |
| 3 | `shithub-backup`   | s-1vcpu-2gb ($12/mo) | `shithub`, `shithub-backup`|
| 4 | `shithub-monitoring`| s-2vcpu-4gb ($24/mo)| `shithub`, `shithub-monitoring` |

**Capture the IPs.** For each droplet, write down both the
**public IPv4** (for SSH from your laptop, and for `shithub-app`
to point DNS at) and the **private IPv4** (for inter-droplet
traffic — appears under "Private IPv4" in the droplet detail).

**Now go back to Phase A5** and update the `A` records for `@`
and `www` to point at `shithub-app`'s public IPv4. The CNAME
for `docs` you set already; it will start resolving once
shithub-docs CDN is ready (~1 min).

### B4. Create + attach the block volume

1. Left sidebar → **Volumes** → **Create Volume**.
2. **Region:** NYC3.
3. **Size:** **100 GB** ($10/mo).
4. **Filesystem format:** Ext4. **Mount options:** automatic.
5. **Attach to droplet:** `shithub-app`.
6. **Mount point:** **`/mnt/shithub_data`** (DO's default for the
   volume name).
7. Create + attach.

**Verify (after SSHing in B5):**
```sh
df -h /mnt/shithub_data    # should show ~100 GB Ext4 mounted
```

We'll move the mount to `/data` (where the playbook expects it)
during Phase C bind-mount step.

### B5. SSH-bootstrap — confirm you can reach all four droplets

From your laptop:

```sh
# Replace IPs with the public IPv4 of each droplet.
ssh root@<APP-IP>          # shithub-app
ssh root@<DB-IP>           # shithub-db
ssh root@<BACKUP-IP>       # shithub-backup
ssh root@<MONITORING-IP>   # shithub-monitoring
```

If `Permission denied (publickey)`, your SSH key didn't get
attached at create time; add it via DO console (Droplet →
Access → Reset Root Password is the only fallback that works
without an existing key).

**Verify:** `whoami` returns `root` on each.

### B6. Bootstrap inter-droplet SSH

Ansible will use shithub-app as its control node (we install it
there in Phase C). For Ansible to reach the other three over
the private network, shithub-app needs an SSH key authorized on
each.

On **shithub-app**:

```sh
ssh-keygen -t ed25519 -f /root/.ssh/id_ed25519 -N ""
cat /root/.ssh/id_ed25519.pub
```

Copy that public key. On **shithub-db**, **shithub-backup**, and
**shithub-monitoring**:

```sh
mkdir -p /root/.ssh
cat >> /root/.ssh/authorized_keys <<'EOF'
<paste shithub-app's pubkey here>
EOF
chmod 600 /root/.ssh/authorized_keys
```

**Verify (from shithub-app):**
```sh
ssh root@<DB-PRIVATE-IP> hostname           # → shithub-db
ssh root@<BACKUP-PRIVATE-IP> hostname       # → shithub-backup
ssh root@<MONITORING-PRIVATE-IP> hostname   # → shithub-monitoring
```

If asked about host key fingerprints, say yes.

---

## Phase C — Hand Claude Code the keyboard

### C1. Install Claude Code on shithub-app

On **shithub-app**:

```sh
apt-get update
apt-get install -y curl ca-certificates
curl -fsSL https://claude.ai/install.sh | sh
```

Then run `claude` and authenticate with the same Anthropic
account you're using on your laptop (browser flow on your
laptop, paste the code into the SSH terminal).

### C2. Install build dependencies on shithub-app

These are needed to build the `shithubd` binary on the droplet:

```sh
apt-get install -y \
  git make build-essential \
  ansible \
  golang-go
# Verify Go ≥ 1.22 (the project targets 1.26).
go version
```

If the apt Go is too old (Ubuntu 24.04 ships ~1.22 which is
fine), skip the next part. If you need a newer Go, install via
the tarball method:

```sh
curl -LO https://go.dev/dl/go1.22.5.linux-amd64.tar.gz
rm -rf /usr/local/go
tar -C /usr/local -xzf go1.22.5.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> /root/.bashrc
source /root/.bashrc
go version
```

### C3. Clone the source

```sh
mkdir -p /root/src && cd /root/src
git clone https://github.com/tenseleyFlow/shithub.git
cd shithub
git log --oneline -3      # confirm latest commit
```

### C4. Move the volume mount to /data

The Ansible playbook expects `/data` as the data root. We could
edit the inventory, but it's cleaner to bind-mount the
DO-attached volume to `/data`:

```sh
mkdir -p /data
mount --bind /mnt/shithub_data /data
echo '/mnt/shithub_data /data none bind 0 0' >> /etc/fstab

# Verify the bind mount survives the next reboot test:
mount | grep -E '/(data|mnt/shithub_data)'
```

### C5. Hand off to Claude Code

From your SSH session on shithub-app, run:

```sh
cd /root/src/shithub
claude
```

When Claude is up, paste this priming message:

> This is the shithub deploy you've been planning. The repo is at
> github.com/tenseleyFlow/shithub. You wrote the sprint specs at
> .docs/sprints/, especially S37-deploy.md and S40-launch.md.
> We are at Phase D of `deploy/cutover/SETUP-GUIDE.md` — please
> read that file and the prerequisite docs, then walk me through
> Phase D with my hands on the keyboard. Domain is shithub.sh.
> Postmark is set up. Spaces buckets are: shithub-prod (NYC3,
> private), shithub-prod-dr (SFO3, private), shithub-docs (NYC3,
> public). Volume bind-mounted at /data. Other droplets reachable
> via private IPs.

Claude will pick up from there. **The rest of this guide is
written for Claude (or you, if you'd rather drive yourself).**

---

## Phase D — Inventory + secrets

### D1. Copy the production inventory template

```sh
cd /root/src/shithub
cp deploy/ansible/inventory/production.example deploy/ansible/inventory/production
```

The bare name `production` is gitignored so secrets stay out of
the repo.

### D2. Generate the cryptographic secrets

```sh
# Session signing key (cookie MAC).
openssl rand -base64 32 > /tmp/session_key
# TOTP AEAD key (encrypts 2FA secrets at rest).
openssl rand -base64 32 > /tmp/totp_key
# Postgres passwords.
openssl rand -base64 24 > /tmp/db_password
openssl rand -base64 24 > /tmp/hook_password
# WireGuard private keys (one per host).
for h in app db backup monitoring; do
  wg genkey > /tmp/wg_${h}.key
  wg pubkey < /tmp/wg_${h}.key > /tmp/wg_${h}.pub
done
```

### D3. Fill in the inventory

```sh
$EDITOR deploy/ansible/inventory/production
```

Fill in:
- `app_host`, `db_host`, `backup_host`, `monitoring_host` — the
  **private IPv4** of each droplet.
- `domain: shithub.sh`
- `caddy_email: ops@shithub.sh` (Let's Encrypt notifications)
- `db_password`, `hook_password` — paste from `/tmp/`.
- `session_key`, `totp_key` — paste from `/tmp/`.
- `s3_endpoint: nyc3.digitaloceanspaces.com`
- `s3_region: us-east-1` (Spaces uses this for SigV4)
- `s3_bucket: shithub-prod`
- `s3_access_key_id`, `s3_secret_access_key` — from B2 step 5.
- `email_backend: postmark`
- `postmark_server_token` — from A4.
- `email_from: shithub <noreply@shithub.sh>`
- `auth_base_url: https://shithub.sh`
- WireGuard peer keys from `/tmp/wg_*.{key,pub}`.

After filling in:

```sh
chmod 600 deploy/ansible/inventory/production
shred -u /tmp/session_key /tmp/totp_key /tmp/db_password \
        /tmp/hook_password /tmp/wg_*.key
```

(Keep the public WireGuard keys in case you need to add a peer
later; the private keys are now only in the inventory.)

---

## Phase E — Deploy

### E1. Dry-run

```sh
cd /root/src/shithub
make deploy-check ANSIBLE_INVENTORY=production
```

Read the diff. Expect every host to be `changed`. If any host
shows `unreachable`, the SSH bootstrap from B6 missed a droplet.

### E2. Build the binary with the version stamp

We're not cutting the v0.1.0 tag yet — that's a launch-day
ceremony. For this first deploy, the binary will stamp the
short commit + build time, which is fine; the soft-launch
window catches surprises before we tag.

```sh
make build
./bin/shithubd version
# expect:
#   Version: <short-commit-or-dev>
#   Commit:  <short-commit>
#   Built:   <today, UTC>
```

### E3. Apply

```sh
make deploy ANSIBLE_INVENTORY=production
```

Expect ~15–30 min on the first run. The roles run in this order:
**base → postgres → shithubd → caddy → wireguard → backup →
monitoring-client.** Caddy will obtain real Let's Encrypt certs
on first request; `caddy_use_acme_staging` should be `false` in
the inventory so we get a real cert.

If a role fails, **stop**. Re-running with `--limit` and the
specific role tag is the surgical path. Read the journal of the
failing service before retrying:
```sh
journalctl -u <service> -n 200
```

### E4. Bootstrap the admin (you)

```sh
ssh root@<APP-PRIVATE-IP>
sudo -u shithub /usr/local/bin/shithubd admin bootstrap-admin \
  --email you@your-current-email.com
```

The CLI prints a one-time password-reset link. Open it in a
browser, set a password, **immediately enable 2FA**.

### E5. Smoke

```sh
deploy/cutover/smoke.sh https://shithub.sh
```

If everything is green, the soft launch is live. Signups are
still gated (we set `SHITHUB_AUTH__SIGNUP_DISABLED=true` in the
inventory by default for soft launch); you're the only user.

---

## Phase F — Soft launch (24–48h)

You're now using shithub yourself. Things to do:

1. **Push the project's source.** Create the `shithub` org,
   create the `shithub` repo under it, push from this checkout.
2. **Walk every flow.** Signup (toggle the gate, verify, gate
   again), password reset, 2FA, SSH key add (the SSH transport
   isn't shipped, so this just stores the key — fine), PAT
   create, repo create, push, issue, PR, review, merge, search.
3. **Note every rough edge.** File issues against
   `shithub/shithub` itself.
4. **Watch the dashboards.** Grafana on the monitoring host.
5. **First daily backup runs at the next 03:00 UTC.** Confirm
   it landed in Spaces the morning after.
6. **First restore drill.** When the second daily backup
   exists, run the drill.

Day 2 morning: you have a real instance with real data + a real
backup. You're as ready as you'll ever be.

---

## Phase G — Public launch

### G1. Tag v0.1.0

```sh
cd /root/src/shithub
git tag -a v0.1.0 -m "v0.1.0 — initial public release"
git push origin v0.1.0
```

### G2. Build and re-deploy with the tagged version

```sh
git fetch --tags
git checkout v0.1.0
make build
./bin/shithubd version    # Version: v0.1.0
make deploy ANSIBLE_INVENTORY=production
```

Now the home page's Version field reads `v0.1.0`. Verify in a
browser.

### G3. Open signups

Edit the inventory:
```sh
$EDITOR deploy/ansible/inventory/production
# Change: signup_disabled: false
make deploy ANSIBLE_INVENTORY=production ANSIBLE_TAGS=app
```

### G4. Update the status page

```sh
$EDITOR docs/public/status.md
# Update timestamp + "All systems normal."
make docs                       # local mdBook build
deploy/docs-site/sync-to-spaces.sh
```

### G5. Post the announcement

`docs/blog/v0.1.0-launch.md` is the copy. Submit to:
- Hacker News
- /r/programming, /r/selfhosted
- lobste.rs
- Mastodon

You're live. Read `docs/internal/runbooks/day-one.md` next.

---

## When something goes wrong

- **Caddy won't get a cert.** DNS hasn't fully propagated, or
  port 80 is blocked. Check both. Last resort: flip
  `caddy_use_acme_staging=true`, redeploy, get the staging cert
  to confirm everything else works, then flip back when DNS is
  good.
- **`/readyz` returns 503.** DB or storage unreachable. Check
  `journalctl -u shithubd-web` for the specific error.
- **Postmark says emails go through but they don't arrive.**
  Check spam, then check Postmark's Activity tab — every send
  is logged. DKIM may not be propagated yet; first 24h is rough
  on cold-start deliverability.
- **Ansible reports "host unreachable" mid-run.** SSH from
  shithub-app to the failing host's private IP; if that doesn't
  work, you're missing the key in B6.

Anything else: read `docs/internal/runbooks/incidents.md` and
the `troubleshooting.md` in self-host docs.
