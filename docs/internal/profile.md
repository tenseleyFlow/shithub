# User profile

S09 shipped the public `/{username}` page and the `/avatars/{username}` route. Later sprints added edit-profile UI, pinned repositories, a profile README card, and a local contribution calendar so the overview follows GitHub's profile layout.

## Routes

| Route | Source | Notes |
|---|---|---|
| `GET /{username}` | profile.serveProfile | Public profile. citext lookup; canonical-case 301; reserved short-circuit. |
| `POST /{username}/pins` | profile.pinsUpdate | Auth required. Profile owner saves up to six public owned repositories. |
| `GET /avatars/{username}` | profile.serveAvatar | Streams uploaded avatar OR falls back to deterministic SVG identicon. |

`/{username}` is the **catch-all** — chi matches static routes in registration order, so the wildcard is registered last via the `ProfileMounter` hook. The reserved-name list (`internal/auth/reserved.go`) is the second line of defense if a future top-level route is added but not registered before the wildcard.

## Behavior summary

| Request | Response |
|---|---|
| `/alice` (exists) | 200 + profile page |
| `/Alice` (canonical is `alice`) | 301 → `/alice` |
| `/oldname` (renamed → `alice`) | 301 → `/alice` (via `username_redirects`) |
| `/badactor` (suspended) | 410 + "account unavailable" page |
| `/no-such-user` | 404 styled error |
| `/login` (reserved) | route handler runs (NOT the profile catch-all) |
| `/admin` (reserved, no handler) | profile handler short-circuits with 404 |
| `/avatars/alice` (no upload) | 200 + identicon SVG |
| `/avatars/alice` (uploaded) | 200 + object-store stream |
| `/avatars/no-such-user` | 200 + identicon (no existence leak) |

## Suspended users — 410 not 404

Suspended (or soft-deleted-during-grace) users render the dedicated "account unavailable" template with status 410 (Gone). Choices behind that:

- 404 would lie about existence — letting an attacker probe whether a name was ever taken.
- 200 would imply a normal profile, confusing crawlers and humans.
- 410 is semantically "this resource existed, it's gone now" — accurate for suspension, neutral about reason.

The template never reveals *why* the user is suspended.

## Casing redirects

The profile handler stores the canonical (DB) casing and 301-redirects when the request path differs. The redirect happens **after** the DB lookup confirmed the user exists, so a typo never loops:

- `/Alice` → DB returns user with `username = "alice"` (citext) → 301 → `/alice`
- `/zzzzz` → DB miss → tryRedirectOrNotFound → 404 (no redirect)

## Username redirects

When S10 ships username-change, the old name is recorded in `username_redirects (old_username, user_id, changed_at)`. On profile lookup miss, we consult that table:

- Hit → 301 to the new canonical name.
- Miss → 404.

The redirect record itself doubles as a 30-day reservation — the signup path will check this table when validating new usernames (handled in S10).

## Identicons

`internal/avatars.Identicon(username, pixelSize)` returns a deterministic SVG built from `sha256(lowercase(username))`:

- 5×5 grid, mirrored horizontally so every identicon is symmetric.
- Color picked from a fixed 12-entry palette derived from Primer accent ramps.
- Same input → same output. No upload needed.
- No user input ever reaches the SVG body — only sha256-derived bytes.

`X-Content-Type-Options: nosniff` is set on the response as defense in depth.

## Avatar upload (placeholder)

S09 does not implement upload. The avatar route reads `users.avatar_object_key`:

- NULL → identicon
- Set + ObjectStore wired → stream from object store with `Cache-Control: public, max-age=31536000, immutable`
- Set + ObjectStore nil → identicon (graceful fallback)

S10 will write the upload path, including resize-to-variants (`avatars/<owner>/<size>.<ext>`) and EXIF stripping.

## Privacy

The profile renders only fields the user explicitly set (or that the system computed publicly):

- Username, display name, bio, location, company, website, pronouns, joined date, avatar.
- Organizations the viewer can already discover through membership/profile navigation.
- Public pinned repositories and visible repository activity.
- **Email addresses NEVER appear on the profile page.** Post-MVP opt-in only.
- Verified-email status is exposed via the API (`/api/v1/user`), not the public page.

## Profile README

The overview looks for a visible repository owned by the user whose name matches the username, then renders a root-level `README*` file in a GitHub-style bordered card:

- Markdown is rendered only through `internal/markdown.RenderDocumentHTML`.
- Plain non-Markdown README files are HTML-escaped and shown in a `<pre>`.
- Relative links and images are rewritten to the matching repository `blob` or `raw` route on the repository default branch.
- The self-view edit pencil links to the actual repository default branch, not a hard-coded `trunk`.
- Visibility is checked with `policy.IsVisibleTo`; private profile READMEs only render to actors who can already see that repository.

## Contribution calendar

The overview contribution calendar is computed from local Git history for repositories visible to the viewer:

- The window is the last 365 days, rendered as a 53-week GitHub-style grid.
- Only visible repositories are scanned, capped at 80 repos and 2,000 commits per repo for request-time safety.
- When the user has verified email addresses, commits are counted only if the author email matches one of them.
- If no verified email exists, shithub falls back to visible user-owned repository commits as a best-effort local signal.
- Private repository contributions appear only when the viewer can already see those repositories.

## Self-view enrichment

When the viewer's session matches the profile's user (`viewer.ID == user.ID`):

- A small "you" badge renders next to the display name.
- An "Edit profile" button links to `/settings/profile` (S10).
- A "Customize pins" modal lists the user's public repositories with a
  live client-side filter and persists up to six selected repos through
  `profile_pin_sets` / `profile_pins` (migration 0040).
- Cache-Control flips from `max-age=300` (anonymous) to `no-cache, private` so admin- or settings-driven changes appear immediately.

Pinned repositories are intentionally public-only. Private repos are
not offered in the picker and saved pin IDs are revalidated against the
current public owner repo list before write. A `profile_pin_sets` row
records that the owner customized the set, so "zero pins" is distinct
from "never customized."

## OG metadata

The layout reads `OGTitle`, `OGDescription`, `OGImage` from the page data and emits the corresponding meta tags. The profile handler populates all three:

- Title = `"<display name> (@<username>)"`
- Description = bio if set, else `"@<username> on shithub"`
- Image = avatar URL (`/avatars/<username>`)

Empty values omit the tag entirely.

## Caching policy

| View | Cache-Control |
|---|---|
| Anonymous profile | `max-age=300` |
| Self-view | `no-cache, private` |
| Identicon | `public, max-age=86400` |
| Uploaded avatar | `public, max-age=31536000, immutable` |

The "immutable" on uploaded avatars is safe because S10 will store under a content-addressed key (`avatars/<owner>/<sha256>.<ext>`); when the image changes, the URL changes, so the old URL never needs to invalidate.

## Sprint-test discipline

`internal/web/handlers/profile/profile_test.go::TestReservedNameList_HasReasonableContents` enforces a discipline: every top-level path segment shithub registers as of this sprint MUST be on the reserved list. When a future sprint adds a new top-level route, this test fails until `internal/auth/reserved.go` is updated.

Other tests cover:

- Existing-user 200 with rendered fields.
- Unknown-user 404.
- Casing redirect 301.
- Username-change redirect 301.
- Suspended user 410 with the unavailable template.
- User and organization pin customization, including public-only
  candidate filtering and owner-only writes.
- Reserved-name path with a registered handler routes there (NOT the wildcard).
- Reserved-name path without a registered handler short-circuits with 404 (NOT a DB lookup).
- Avatar identicon falls back when no key is stored.

## Pitfalls / what to remember

- **Wildcard ordering** is everything. `/{username}` MUST be registered after every static top-level route. Defense in depth via the reserved-name list catches drift.
- **Casing redirect must not loop.** Match on canonical case ONLY after a successful lookup confirmed the user exists.
- **Avatar route must not 404 on missing user.** It silently falls back to the identicon — leaking less existence info.
- **Suspended page is 410, not 404.** Picked deliberately for the privacy/honesty trade-off.
- **Profile data is public.** Don't add private fields without a feature gate.

## Related docs

- `docs/internal/auth.md` — email/password auth (S05).
- `docs/internal/storage.md` — avatar object-store path conventions (S04).
- `docs/internal/tokens.md` — `/api/v1/user` PAT-authenticated profile JSON (S08).
