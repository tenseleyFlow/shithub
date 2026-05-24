# Licenses

Discovery surface for the curated SPDX license catalog the server
recognizes at repo-create time (`repo.license_template`). The catalog
is static and case-insensitive: `mit`, `MIT`, and `Mit` all resolve to
the canonical row.

## List licenses

```
GET /api/v1/licenses
```

Required scope: none (public).

Returns the curated catalog as a JSON array. Each row carries the
lowercase `key` (what `repo create --license` accepts), the canonical
SPDX `spdx_id` (what GET responses surface), and a human `name`.

### Response

```json
[
  {
    "key": "mit",
    "spdx_id": "MIT",
    "name": "MIT License"
  },
  {
    "key": "apache-2.0",
    "spdx_id": "Apache-2.0",
    "name": "Apache License 2.0"
  }
]
```

## Get a single license

```
GET /api/v1/licenses/{key}
```

Required scope: none (public).

`{key}` is matched case-insensitively against `spdx_id`. `mit`, `MIT`,
and `Mit` all resolve to the same row. Unknown keys return 404.

### Response

```json
{
  "key": "mit",
  "spdx_id": "MIT",
  "name": "MIT License"
}
```
