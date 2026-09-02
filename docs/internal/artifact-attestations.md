# Artifact attestations

Repository artifact attestations are shithub's baseline provenance storage
surface for SP29. They deliberately start as manual repository uploads, not as
automatic Actions runner emission.

## User surface

Repositories expose the page at:

- `/{owner}/{repo}/security/attestations`
- `/{owner}/{repo}/security/attestations/{attestationID}/download`

Repository writers can upload an in-toto Statement JSON document up to 1 MiB.
Repository readers can list and download stored statements when the billing
gate allows the repository surface.

Accepted documents must be JSON objects with:

- `_type` containing `in-toto.io/Statement`
- at least one `subject` entry with `name`
- at least one subject digest, stored as `algorithm:hex`
- `predicateType`
- a non-null `predicate`

The server compacts the JSON before storage and records the first subject name,
normalized digest, predicate type, byte count, uploader, and creation time.

## Storage

`repo_artifact_attestations` stores one row per uploaded statement:

- `repo_id` references `repos(id)` with `ON DELETE CASCADE`
- `source_run_id` optionally references `workflow_runs(id)` with
  `ON DELETE SET NULL`
- `uploaded_by` optionally references `users(id)` with `ON DELETE SET NULL`
- `statement` is `json` and must be a JSON object. `json`, not `jsonb`:
  the column has to hand back the exact bytes the server compacted, since
  `byte_count` describes them and DSSE-style verification is over them.
  `jsonb` re-canonicalizes key order and whitespace on write.

The optional run reference is reserved for a later Actions publishing flow. The
current web surface leaves it null.

## Billing and access

Public repositories and personal repositories can list, upload, and download
attestations at baseline, subject to normal repository read/write policy.

Private organization repositories require the Team
`artifact_attestations` entitlement before listings, uploads, or downloads are
available. Denied private-org downloads return `402 Payment Required`; denied
uploads render the repository page with the Team upgrade message.

## Deferrals

This surface does not yet:

- generate attestations from workflow runs;
- verify signatures or DSSE envelopes;
- expose a REST API;
- bind an attestation to a release asset/package upload automatically; or
- publish bundle formats beyond raw in-toto Statement JSON.

Those belong in the later Actions/supply-chain provenance sprint. Until then,
the Actions Attestations tab remains an honest workflow empty state rather than
a claim of automatic provenance generation.
