# GPG keys & commit signing

Uploading an OpenPGP public key lets shithub mark your signed
commits and tags as **Verified**. Verification runs server-side
against the bytes git stored in each commit object — there is no
client-side trust ceremony.

## 1. Generate an OpenPGP key

Skip this if you already have a signing key (`gpg --list-secret-keys --keyid-format=long`
shows what you have).

```sh
gpg --full-generate-key
```

Pick `ECC (sign and encrypt)` and `Curve 25519` when prompted —
ed25519 is the modern default and matches what `gh` recommends.
The key must carry **at least one user ID** with an email address
that you have verified on your shithub account, or the resulting
signatures will fall back to the `unverified_email` reason.

> Encryption-only keys (e.g. a key generated for `--encrypt`
> with no signing subkey) are accepted by shithub, but they can't
> verify commits. The REST response surfaces `can_sign: false`
> honestly when this is the case.

## 2. Export the public key

```sh
gpg --armor --export <KEY-ID-OR-EMAIL>
```

Copy the entire ASCII-armored block, including the
`-----BEGIN PGP PUBLIC KEY BLOCK-----` and `END` lines.

## 3. Add the key in shithub

Settings → **SSH and GPG keys** → "New GPG key". Paste the block,
give it a label (e.g., "laptop"), save.

The page shows the primary fingerprint shithub parsed; verify it
matches `gpg --fingerprint <KEY-ID>` locally before relying on
the badge.

## 4. Tell git to sign

```sh
git config --global user.signingkey <KEY-ID>
git config --global commit.gpgsign true
git config --global tag.gpgsign true
```

Use the email on your shithub account as `user.email` — the
verification cross-check compares the signature's UID emails
against your account's verified emails.

## 5. Push and see the badge

After your next signed push, the commit list, the single-commit
page, and the tag list all show a green **Verified** pill. Click
it for signer + verified-at details.

## What the badge states mean

| Pill | Reason | Meaning |
|------|--------|---------|
| Green "Verified" | `valid` | Signature parsed, cryptographically checked against a registered key, signing email matches a verified email on the key. |
| Yellow "Unverified" | `unknown_key` | Signature parsed but no uploaded key matches the signing subkey's fingerprint. |
| Yellow "Unverified" | `unverified_email` | Signature is valid for an uploaded key, but the signing email isn't verified on that key's account. |
| Yellow "Unverified" | `bad_email` | Signature is valid for an uploaded key, but the signing email isn't on the key at all. |
| Yellow "Unverified" | `expired_key` | Signature is valid, but the key was expired at signing time. |
| Yellow "Unverified" | `not_signing_key` | The key referenced isn't a signing key (capability bits missing). |
| Yellow "Unverified" | `malformed_signature` | The signature block didn't parse. |
| Yellow "Unverified" | `invalid` | Signature parsed but the cryptographic check failed. |
| _no badge_ | `unsigned` | Git stored no signature header. This is the default; we don't render anything. |

## Retroactive verification

Uploading a key kicks off a background job that re-scans your
existing commits across every repo and stamps the verification
cache for the matches. Refresh the commit list a moment after
upload — the badges appear without you doing anything.

## Removing a key

Settings → **SSH and GPG keys** → "Delete" next to the GPG key
row. The verification cache rows that resolved against the
deleted key are invalidated; affected commits revert to no
badge until another matching key is uploaded.
