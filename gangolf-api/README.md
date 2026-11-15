# Gangolf API media catalog

Clips and triggers are managed through `/api/v3/clips` and `/api/v3/triggers`. The catalog contains mutable metadata, not downloaded media. It does not execute triggers or play videos.

| Resource | Collection | Item |
| --- | --- | --- |
| Clips | `GET`, `POST /api/v3/clips` | `GET`, `PUT`, `DELETE /api/v3/clips/{id}` |
| Triggers | `GET`, `POST /api/v3/triggers` | `GET`, `PUT`, `DELETE /api/v3/triggers/{id}` |

Use `Authorization: Bearer …` with an existing API credential or JWT. Query API keys are rejected. The configured catalog tenant's enabled host credential and existing admin credentials can access the catalog; other hosts receive 403. API-key permissions remain scoped to the key. The existing messages and Twitch authorization configuration stays in use.

## Representations

Create a clip with `Content-Type: application/json`:

```json
{"provider":"youtube","remoteVideoId":"abcdefghijk","startSeconds":1.5,"endSeconds":4,"title":"Example"}
```

A clip response also includes its server-assigned string `id`. `provider` must be `youtube`; `remoteVideoId` must be an 11-character YouTube ID using letters, digits, `_` or `-`. URLs are rejected and never fetched. Both times are nullable finite nonnegative seconds. A nonnull end must exceed the start, or zero when the start is null. `title` is nullable and limited to 256 Unicode code points. Missing optional fields become null.

Create a trigger with:

```json
{"text":"example","clipId":"102"}
```

A trigger response also contains its string `id`. Text is nonblank, valid UTF-8, limited to 256 Unicode code points, and preserved exactly. It is literal metadata; duplicate texts and duplicate text/clip pairs are allowed. `clipId` must name an existing catalog clip. New or replaced triggers cannot have null references.

Imported resources additionally expose immutable `legacy` provenance containing `snapshot`, `archiveNodeId` and `legacyId`. Trigger provenance also contains `legacyVideoId` and `initialResolution`. Fifteen imported triggers initially expose `clipId: null` because their original target is absent. PUT can repair these references while retaining the original unresolved provenance. Frozen archive raw records remain the authority for original absent/null distinctions.

PUT fully replaces mutable fields. Required fields are `provider`/`remoteVideoId` for clips and `text`/`clipId` for triggers. Optional fields omitted from PUT become null. A GET response can be sent back: `id` and `legacy`, if supplied, must exactly match their current server values. POST rejects client-assigned `id` or `legacy`. Unknown or duplicate JSON fields are rejected. The request body limit is 1 MiB.

POST returns 201 and `Location`; GET and PUT return 200. DELETE returns 204 and removes only the active entry. Missing item operations, including repeated DELETE, return 404. Deleting a clip referenced by any current trigger returns 409 `clip_in_use`; reassign or delete those triggers first. IDs increase independently in each collection and are never reused by the API.

## Listing

Both collections support:

| Query | Meaning |
| --- | --- |
| `count` | Page size, default 100, range 1–500 |
| `continue` | Nonnegative offset, default 0 |
| `sort` | `id` only, default `id` |
| `descending` | `true` or `false`, default `false` |

Clips additionally filter by `provider` and `remoteVideoId`. Triggers filter by `clipId`, exact case-sensitive `text`, and `unresolved=true|false`. Filters combine with AND. `unresolved` describes the current null reference, not the immutable initial resolution. Unknown, malformed or repeated parameters return 400. Numeric IDs determine order before pagination.

```json
{"result":[],"total":0,"continue":null}
```

`total` counts filtered records. `continue` is the next offset, or null at the end. Offset pages reflect a live collection; simultaneous edits can move records between requests.

## Errors and retries

Errors use `{"code":"…","text":"…"}` and responses use `Cache-Control: no-store`.

| Status | Codes |
| --- | --- |
| 400 | `invalid_id`, `invalid_filter`, `invalid_clip`, `invalid_trigger`, `invalid_body` |
| 401 / 403 | `unauthorized` / `forbidden` |
| 404 | `not_found`, `clip_not_found` |
| 405 | `method_not_allowed`, with `Allow` |
| 409 | `clip_in_use`, `catalog_capacity_exceeded` |
| 413 / 415 | `body_too_large` / `unsupported_media_type` |
| 503 | `media_unavailable`, `storage_unavailable`, `write_outcome_unknown`, `auth_unavailable` |

After a submitted catalog write, the API reads back and compares the intended catalog. An ambiguous failure returns `write_outcome_unknown` without retrying the write. POST is nonidempotent: reconcile by reading before deciding whether to create again.

## Installation and one-time import

Keep the primary DiVoid token, root, messages and auth configuration unchanged. Provision a separate private owner-4 project named `Gangolf Media` and one private, embedding-disabled node named `catalog`, type `gangolf-media-catalog`, with `rootNodeId` equal to that project. Their only incident edge must be their plain membership link. Do not link either node to the immutable archive.

Configure all four bindings:

- `GANGOLF_MEDIA_DIVOID_TOKEN_FILE`: absolute path to a private credential file, owned by the API process user, regular, single-link, nonsymlink, mode 0600. Its three assignments are `User`, `UserId` (4), and `Key`. Values are not API fields or log output.
- `GANGOLF_MEDIA_ROOT_ID`: the new project ID.
- `GANGOLF_MEDIA_CATALOG_ID`: the new catalog node ID.
- `GANGOLF_MEDIA_TENANT_USER_ID`: the existing enabled host user, currently 42 in the accepted deployment.

The media client uses the existing DiVoid URL, timeouts and redirect policy. All four absent keeps media disabled with authenticated requests returning 503. Partial or invalid configuration fails startup. The mutable document is bound to the primary auth-config ID and explicit tenant on every read.

With the service stopped, run the API binary in one-shot mode using the same primary and media environment:

```sh
./gangolf-api seed-media \
  -archive-root 145 -videos-group 151 -triggers-group 149 \
  -snapshot dc6a9028e5d7941efd8d9a31990690e9175dca8646619335b1c351451d32e1ce
```

This command exits without opening a listener or starting Twitch. It reads only archive controls and the video/trigger groups, validates metadata, record hashes and exact relationships, and writes one complete catalog. The accepted snapshot projects 90 clips and 122 triggers: 107 resolved and 15 unresolved references. Output is `initialized` or `already_initialized`. A conflicting or malformed target is refused. Rerunning the same initialization preserves later edits/deletions and performs zero writes. There is no automatic seed during normal startup or HTTP requests.

After authorized installation, paginate both collections and verify those totals, retrieve one of each, and exercise CRUD with synthetic records. Verify clip deletion is restricted until its synthetic trigger is removed. Remove synthetic records in dependency order. Do not repair historical dangling records without a separate decision. Independently verify the frozen archive and existing live controls remain unchanged.

The catalog is limited to 8 MiB. Each accepted mutation replaces it once under a process mutex. Run exactly one API writer; run seed only while that service is stopped. There is no distributed compare-and-swap or protection against external generic graph writers. Multi-instance writers or a larger catalog require a revised storage contract.
