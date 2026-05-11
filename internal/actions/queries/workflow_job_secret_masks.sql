-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: UpsertWorkflowJobSecretMask :exec
INSERT INTO workflow_job_secret_masks (job_id, ciphertext, nonce)
VALUES ($1, $2, $3)
ON CONFLICT (job_id) DO UPDATE
SET ciphertext = EXCLUDED.ciphertext,
    nonce      = EXCLUDED.nonce,
    created_at = now();

-- name: GetWorkflowJobSecretMask :one
SELECT job_id, ciphertext, nonce, created_at
FROM workflow_job_secret_masks
WHERE job_id = $1;
