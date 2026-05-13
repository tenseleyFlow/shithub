# gpgkey testdata

Two committed fixtures — real `gpg`-produced ASCII-armored blocks
serving as a regression baseline. The bulk of `parse_test.go` still
synthesizes fixtures in-memory via `github.com/ProtonMail/go-crypto/openpgp`
(no `gpg` dependency in CI, deterministic, no time-bomb expiry races),
but these two files exercise the **codec compatibility** with real-world
output from `gpg (GnuPG)`:

- `ed25519.asc` — `gpg --quick-gen-key 'shithub-test-ed25519 <ed25519@shithub.test>' default default 0` then `--armor --export`. ed25519 primary + curve25519 encryption subkey, no expiry.
- `rsa4096.asc` — `gpg --quick-gen-key 'shithub-test-rsa <rsa@shithub.test>' rsa4096 default 0` then `--armor --export`. RSA-4096 primary + RSA-4096 encryption subkey.

Both are throwaway keys; no real-user material is ever committed here.

If a future bug surfaces from a specific real-world key shape, drop the
producing key here as `<shape>.asc` and reference it from
`parse_test.go` via `os.ReadFile`. Generation recipe (uses an isolated
GNUPGHOME so it doesn't pollute the host keyring):

```bash
TMPHOME=$(mktemp -d) && chmod 700 "$TMPHOME"
export GNUPGHOME=$TMPHOME
gpg --batch --pinentry-mode loopback --passphrase '' \
    --quick-gen-key '<your-test-id> <email@shithub.test>' <algo> <usage> 0
gpg --armor --export <email@shithub.test> > <shape>.asc
rm -rf "$TMPHOME"
```
