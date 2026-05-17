# User automation

Personal automation endpoints use PAT authentication. Read routes require
`user:read`; create, delete, and disable routes require `user:write`.

## Cron workflow dispatches

Cron dispatches let a Pro user schedule `workflow_dispatch` runs for a
personal repository without committing a workflow `schedule:` block.
The create route only accepts repositories owned by the authenticated
user.

```
GET    /api/v1/user/cron-dispatches
POST   /api/v1/user/cron-dispatches
DELETE /api/v1/user/cron-dispatches/{id}
POST   /api/v1/user/cron-dispatches/{id}/disable
```

Create body:

```json
{
  "repo_id": 42,
  "workflow_file": ".github/workflows/ci.yml",
  "ref": "trunk",
  "cron_expr": "15 9 * * 1-5"
}
```

List and create responses contain:

```json
{
  "id": 7,
  "repo_id": 42,
  "workflow_file": ".github/workflows/ci.yml",
  "ref": "refs/heads/trunk",
  "cron_expr": "15 9 * * 1-5",
  "next_fire_at": "2026-05-18T09:15:00Z",
  "last_fire_status": "",
  "disabled": false
}
```

`DELETE` and `disable` return `204 No Content` on success. Missing or
cross-user IDs return `404`.

## Webhook relays

Webhook relays create a personal receiver URL that fans inbound webhook
requests out to configured destinations with relay-side HMAC signing.
Operators must configure the relay secret box before creation is
available.

```
GET    /api/v1/user/webhook-relays
POST   /api/v1/user/webhook-relays
DELETE /api/v1/user/webhook-relays/{id}
POST   /api/v1/user/webhook-relays/{id}/disable
```

Create body:

```json
{
  "name": "staging deploy fanout",
  "destinations": [
    {
      "url": "https://ci.example.test/hooks/deploy"
    }
  ]
}
```

Create responses include the raw relay token exactly once:

```json
{
  "id": 12,
  "name": "staging deploy fanout",
  "token_prefix": "shr_abcdef",
  "destinations": [
    {
      "url": "https://ci.example.test/hooks/deploy"
    }
  ],
  "disabled": false,
  "token": "shr_abcdef123456",
  "receiver_url": "https://shithub.example/webhook-relay/shr_abcdef123456"
}
```

List responses omit `token` and `receiver_url`. `DELETE` and `disable`
return `204 No Content` on success. Missing or cross-user IDs return
`404`.
