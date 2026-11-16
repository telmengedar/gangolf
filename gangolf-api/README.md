# Gangolf API

Clips and triggers are managed through `/api/v3/clips` and `/api/v3/triggers`. The catalog contains mutable metadata, not downloaded media. New eligible messages can produce automatic video intents; the browser overlay handles playback.

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


## Live events

`GET /api/v3/events` opens a live SSE subscription. Prefer `Authorization: Bearer …` with an API key or JWT. URL-only applications can use an explicitly issued `channel-read` token as the single `apiKey` query parameter; it is restricted to owned channel IDs. Do not combine header and query credentials. Native `EventSource` can use that URL. Consume through an authorized same-origin route. The current deployment listens on host loopback; this source change does not configure browser reachability, CORS, TLS, or proxy buffering.

```sh
curl --no-buffer --get "$GANGOLF_API/api/v3/events" \
  --header "Authorization: Bearer $GANGOLF_TOKEN" \
  --header 'Accept: text/event-stream' \
  --data-urlencode 'types=message.created,video.triggered' \
  --data-urlencode 'service=Twitch' \
  --data-urlencode 'channel=101'
```

`types` is required: one or both of `message.created` and `video.triggered`, comma-separated, without duplicates. Optional `service` and `channel` are exact, case-sensitive filters; `channel` requires `service`. Types are ORed, service/channel are ANDed. Omit service/channel to receive all currently readable events. Unknown, repeated, empty, malformed, or unsupported parameters return 400 `invalid_filter`; decoded service/channel have a 256-character limit and the query an 8192-byte limit. Filters never grant permission.

The first flushed frame is:

```text
event: stream.ready
data: {"schemaVersion":1,"replay":false,"resyncRequired":true}

```

Data frames have `event: message.created` or `event: video.triggered` and one compact JSON `data:` line. Their envelope is `{schemaVersion:1,id,type,time,service,channel,channelId,data}`. `id` is an unpredictable process epoch plus a process-local sequence. It is useful for diagnostics/deduplication, never a durable cursor; sequence gaps are normal. `time` is server publication UTC time. `channelId` is a v3 channel ID or null for legacy v2 messages. No SSE `id:` field is sent.

Message data is either `{resourceVersion:3,message:<existing v3 message resource>}` or `{resourceVersion:2,batchId,index,message:<existing v2 message>}`. Legacy `index` is the zero-based batch position. Actual accepted chunks, timestamps, source and delivery fields are preserved. Events cover successful local v3 creation, legacy batches, new Twitch inbound messages, and the first confirmed outbound reservation. Outbound creation honestly has `delivery.state:"unknown"` because the event precedes sending. Retries/replays, terminal delivery changes, PATCH, DELETE, history reads and imports do not emit a second creation event.

Each data frame and every 15-second heartbeat reloads authorization. Key permissions are bounded by the current user grant and never inherit admin from the user. Managed channel events require current ownership or effective admin and an enabled, nondeleted channel. New users cannot read unowned legacy channels. Existing legacy grants retain shared non-Twitch and exact Twitch-profile history rules; disabled Twitch profiles retain historical visibility. Video subscriptions require available current media storage and the same source-channel authorization; receiving video does not grant catalog access. Revocation, expiry, unavailable auth/media storage, disconnect, overflow, ambiguous message creation or shutdown ends the stream; idle revocation is detected on the next heartbeat plus its bounded 10-second authorization operation. Message-only streams do not depend on media availability.

There are 16 streams total and 4 per user across all their keys. A stream can queue 64 events and at most 8 MiB of encoded frames; an individual frame is limited to 8 MiB. Matching slow subscribers are disconnected when they exceed either queue limit; producers and other subscribers continue. Each write/flush has a 5-second deadline. Heartbeat comments keep the connection active. Unsupported flush/deadline writers return 503 `stream_unavailable`; exhausted stream slots return 429 `stream_limit`. Incompatible Accept returns 406; request bodies return 400; non-GET methods return 405. Existing 401/403/503 authorization/storage errors use the normal JSON envelope before streaming begins. Failures after headers normally close the connection.

This is a single-process live feed with no replay, outbox, history, or lossless execution guarantee. `Last-Event-ID` returns 409 `replay_not_supported`. External graph writes and other API processes are not observed. Every EOF/error may mean missed events; reconnect always requires resynchronization, even if no reset frame arrived. Publication follows observed storage success and is not atomic with persistence.

For v3 bootstrap, open the stream and buffer creation data, wait for ready, then fetch authorized v3 message history using the same service/channel filters. Merge history and buffered messages by `message.id`, displaying each ID once, then continue live. Bound the frontend buffer; reset/reconnect if buffering or history fails. Paginated history is not an atomic snapshot of edits/deletions/delivery updates. Legacy v2 history lacks batch/index identity, so reliable history/live deduplication is unavailable for that variant. Video intents have no durable history: never automatically replay or execute old buffered video intents after reconnect.

A minimal bearer fetch reader follows. `receive` handles ready and data frames; the caller owns bounded bootstrap buffering, ID merging, reconnect policy and connection-state display. Cancellation uses an AbortSignal. UTF-8 decoding handles byte fragments, SSE comments are ignored, CRLF is accepted, and frames are bounded. An ended stream throws so the caller starts a fresh bootstrap.

```js
async function readEvents(url, token, receive, signal) {
  const response = await fetch(url, {
    headers: { Authorization: `Bearer ${token}`, Accept: "text/event-stream" },
    signal
  });
  if (response.status !== 200 ||
      !response.headers.get("content-type")?.startsWith("text/event-stream")) {
    await response.body?.cancel();
    throw new Error(`Event connection failed: ${response.status}`);
  }
  const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();
  let pending = "", event = "message", data = [], frameSize = 0;
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) throw new Error("Event connection ended; resynchronize");
      pending += value;
      if (pending.length > 8 * 1024 * 1024) throw new Error("Event frame too large");
      let end;
      while ((end = pending.indexOf("\n")) !== -1) {
        let line = pending.slice(0, end);
        pending = pending.slice(end + 1);
        if (line.endsWith("\r")) line = line.slice(0, -1);
        frameSize += line.length + 1;
        if (frameSize > 8 * 1024 * 1024) throw new Error("Event frame too large");
        if (line === "") {
          if (data.length) await receive(event, JSON.parse(data.join("\n")));
          event = "message";
          data = [];
          frameSize = 0;
        } else if (!line.startsWith(":")) {
          const separator = line.indexOf(":");
          const field = separator < 0 ? line : line.slice(0, separator);
          let text = separator < 0 ? "" : line.slice(separator + 1);
          if (text.startsWith(" ")) text = text.slice(1);
          if (field === "event") event = text;
          if (field === "data") data.push(text);
        }
      }
    }
  } finally {
    await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}
```

## Video event intents

`POST /api/v3/video-events` explicitly publishes a validated `video.triggered` intent. It does not match trigger text, download media, queue playback, or claim a video played. The request accepts exactly two canonical positive decimal string IDs; unknown/duplicate fields, null/missing IDs and client-selected clip/channel/tenant/payload fields are rejected. Use `application/json`, bearer authorization, no query parameters, and at most 1 MiB of body.

```sh
curl "$GANGOLF_API/api/v3/video-events" \
  --header "Authorization: Bearer $GANGOLF_TOKEN" \
  --header 'Content-Type: application/json' \
  --data '{"messageId":"123","triggerId":"9"}'
```

The caller must be the configured media tenant or effective admin, and the source must be a visible nondeleted v3 message with validated relations. Nonadmins need their own enabled managed source channel, or an existing exact legacy Twitch-profile association; catalog ownership alone never grants another user’s channel. Effective admin can publish for a new user’s owned local channel. The current trigger must resolve to an existing clip. The server derives routing and `{messageId,trigger,clip}` from current stored DTOs, then returns 200 with the emitted envelope, even with no subscribers. There is no durable item/Location or event GET history, no storage writes and no automatic text-match requirement. A repeated POST creates a fresh intent. An ambiguous response must not be blindly retried because duplicate intents are possible.

Missing message/trigger returns 404 `not_found`, unauthorized source 403 `forbidden`, unresolved clip 409 `unresolved_trigger`, and absent/unavailable media 503 `media_unavailable`. Existing bearer authentication failures apply. Other methods return 405 `Allow: POST`. Reuse the ordinary clips/triggers API to manage the catalog; archives are never consulted by event production.


## Users, channels and tokens

| Resource | Collection | Item |
| --- | --- | --- |
| Users | `GET`, `POST /api/v3/users` | `GET`, `PUT`, `DELETE /api/v3/users/{id}` |
| User tokens | `GET`, `POST /api/v3/users/{id}/tokens` | `GET`, `DELETE /api/v3/tokens/{id}` |
| Channels | `GET`, `POST /api/v3/channels` | `GET`, `PUT`, `DELETE /api/v3/channels/{id}` |

`current` can replace the user ID for self reads, name updates and token management. Administrative user creation/listing/role changes/deletion require an effective admin credential. Ordinary users can read themselves and replace their own name while preserving `enabled` and `permissions`. PUT accepts the full GET resource. The final enabled admin cannot be disabled, deleted or downgraded. Deleting a user disables authentication and removes its keys; existing message references remain.

Create a user with `{"name":"Alice","enabled":true,"permissions":["host"]}`. The response contains server-assigned `id`, `name`, `enabled` and `permissions`. Existing users without a profile initially have `name:null`; a self PUT or first channel POST explicitly creates their private profile without changing existing permissions or tokens.

Create a channel with `{"service":"Local","channel":"alice"}`. It belongs to the authenticated user; an admin can supply an enabled target `ownerUserId`. Responses include `id`, `service`, `channel`, derived `ownerUserId`, `enabled` and `deleted`. Ownership and service/channel identity cannot change. PUT can change only `enabled`. DELETE is terminal: the canonical service/channel identity stays reserved and cannot be reused. Disabled/deleted channels retain owner-readable history but reject new messages and live events. Register channels before posting as a newly created user.

Channel registration creates an application permission resource. Registering a Twitch channel does not join Twitch, acquire OAuth, enable a profile or start a receiver. Existing operator-managed profile and credential configuration remains necessary for upstream Twitch traffic. That profile’s tenant and canonical channel must agree with the registered owner. Legacy channels cannot be claimed or rehomed through these endpoints.

New lists use `count` (default100,max500), `continue`, `sort=id` and `descending=true|false`. Users and tokens additionally filter by `enabled`; channels filter by exact `service`, `channel` and `enabled`. Filters combine with AND. Responses use `{result,total,continue}`; unknown/repeated filters are rejected.

Issue a normal token with POST `/api/v3/users/current/tokens` and `{"name":"Desktop","scope":"api","permissions":["host"]}`. The successful201 response includes `token` exactly once; later GET/list responses contain only metadata. Tokens retain the existing `id.secret` format, but only a keyed hash is stored. API tokens use headers and default to30days, with a365day maximum. Their permissions are bounded by both issuer and target user. Revocation is DELETE on the token item, including the current token; user disable/delete and role changes apply on each fresh authorization.

For URL-only clients issue `{"name":"Overlay","scope":"channel-read","channelIds":["<eligible-channel-id>"]}`. An eligible channel is either an active channel owned by the target app user or a legacy root channel with an exact enabled trusted Twitch profile for that user. Legacy channels retain their identity and ownerUserId:null; issuing a read key does not adopt or migrate them. Disabling that profile denies these scoped legacy history reads as well as live events; existing generic-key historical behavior stays unchanged. The explicit channel list must be nonempty and unique. To display several channels, list every desired ID when issuing the key; newly registered channels require a new key with an expanded list. These host-only tokens default to24hours with a30day maximum and permit only GET v3 channels/messages/events for the selected eligible channels. Both event kinds use that restriction. Optional `expiresAt` must be future UTC and within the scope limit; expiry is also capped by a finite issuer credential. Read tokens cannot manage users/tokens, write messages, access the catalog or publish video intents.

Use `apiKey=<URL-encoded-channel-read-token>` only on those GET routes. Header+query, repeated/encoded duplicate query credentials, malformed escapes, query JWTs and admin query credentials are rejected. Existing pre-feature nonadmin keys retain message-GET query compatibility. Tokens never appear in Locations, errors or event frames. Responses include `Cache-Control:no-store` and `Referrer-Policy:no-referrer`; external browser/proxy logs can still retain URLs, so use short-lived read scopes and headers whenever possible.

### Initial administrator

Normal startup never creates or elevates an admin. During an explicitly authorized offline writer-quiet window, use the existing primary graph environment and pepper:

```sh
./gangolf-api bootstrap-admin --name Administrator --output /private/new-admin.key
```

The command refuses if an enabled app admin already exists. It creates a separate managed app user and admin token, preserving existing users, keys and storage identities. It opens no listener or receiver. Use a private output directory under trusted operator custody. The output path must not exist; the command creates a regular0600 file, writes and syncs the complete token, closes it and syncs its parent directory before submitting the security activation. Before activation and before reporting success, it verifies that the requested path still names the original regular0600, single-linked, process-owned file containing the saved token. Stdout contains only user ID, key ID and file path after successful independent readback and that final custody check. These bounded checks cannot prevent later deletion by the same operator/UID.

If activation or confirmation is uncertain, the command fails and preserves that exact protected file. Do not delete it or blindly rerun bootstrap: the write may have committed. Recover with the saved token through the normal header-authenticated `GET /api/v3/users/current`, or have the operator inspect the authoritative key/user/profile binding offline. Inconclusive recovery never authorizes reissuance. A fresh exclusive output path may be used only after confirming no admin or intended key was activated. The command does not authorize a production provisioning window by itself.

The API serializes local security mutations and confirms complete config writes by readback. Cross-node changes are not distributed transactions; failures can leave non-authoritative staging profiles. A `write_outcome_unknown` response is not success: reconcile by GET before retrying creation. Run exactly one API writer.


### Renewing an existing administrator key

An authorized operator can issue a fresh finite key for an existing enabled administrator, including after all earlier application admin keys have expired or been lost. Use an explicitly authorized offline writer-quiet window and the existing protected primary graph configuration and pepper:

```sh
./gangolf-api admin-key --user-id <EXISTING_ADMIN_ID> --output <NEW_PRIVATE_FILE>
```

Optional `--expires-at <RFC3339_TIME>` selects a future expiry at most365days from the command’s current time; the default is30days. The target must already have an enabled admin grant and a valid, nondeleted bound profile. This command creates no user/profile, grants no role, and retains every existing key. It issues one fixed `api` key with `admin` and `host` permissions. It requires operator/service authority, not an old app token, so the new lifetime does not inherit an earlier app key’s cutoff.

The output path must be new and inside a trusted operator-controlled private directory. The command reuses bootstrap’s exclusive0600 file, same-file/owner/link/mode/exact-byte checks, file+directory durability and independent auth readback. Raw credential bytes go only to that file. Successful stdout contains `userId`, `keyId`, `expiresAt` and `file`; no token. No listener, receiver, model run or import starts.

If confirmation is uncertain, retain the exact file and reconcile its saved token read-only with ordinary header-authenticated `GET /api/v3/users/current` and `GET /api/v3/tokens/<KEY_ID>`. A possible commit is not permission to blindly issue another key. Failed preactivation custody checks activate nothing; post-submission custody loss returns an unknown outcome. Later destruction by the trusted operator cannot be prevented by these bounded checks. A fresh separately authorized offline issuance can recover lost app credentials while the existing admin grant/profile and primary service authority remain valid; disabled/deleted/downgraded admins and corrupt service state are outside this command.

HTTP token issuance still caps descendants at their authenticating credential’s expiry. Offline renewal adds a fresh operator-authorized finite key; it does not extend or revoke existing keys. After verification, old keys can be revoked through the existing authenticated token DELETE route under separate explicit operational scope. Never run this command concurrently with the API writer or overwrite the initial saved admin key file.


## OBS overlays

Use a channel-read token issued through the existing self/admin token endpoint, then add a Browser Source with `/overlays/chat?apiKey=<URL-encoded-read-token>`. Optional `service` and `channel` select one channel; `channel` requires `service`. Omitting both merges all currently permitted channels explicitly listed in the token, across services. Generic management keys and JWTs are rejected by the HTML entry point. Token-bearing HTML is never templated with the key; first-party scripts use bearer GETs afterward. The fixed public JS/CSS assets reject query strings. The response policy prevents third-party scripts in the credential-bearing page.

Chat opens the live stream before loading the latest 50 authorized v3 messages, merges IDs, and keeps 50 rows. Text and emote codes render as text. Disconnect clears visible state before bounded reconnect; revoked/invalid keys stop the overlay. There is no lossless event replay guarantee.

Automatic video matching only schedules confirmed new local v3 messages and new non-bot Twitch inbound messages. Outbound messages, v2 batches, replay, history, edits and startup scans do not schedule matching. The source must still be active and belong to the configured media tenant by managed ownership or the exact enabled legacy Twitch association. Optional media failures never fail durable chat.

The worker has one goroutine, a 64-ID nonblocking queue and a three-second total attempt deadline. Overflow drops the ephemeral opportunity. It loads a private validated catalog snapshot outside the resource lock; current source/config checks precede emission. A catalog-save generation change, busy media lock, unavailable storage or revoked source drops the attempt without retry. This consistency is process-local under the existing singleton/no-external-writer assumption; it is not a remote graph transaction or durable exactly-once system.

Matching joins text and emote codes in chunk order, trims outer Unicode punctuation/whitespace, lowercases Unicode and collapses whitespace. Internal punctuation remains literal. A whole normalized trigger phrase must match the start or end with a token boundary: `hello` matches `HELLO!` and `say hello`, never `hellothere` or `say hello friend`; `hello world` matches `hello   world!`, never `hello, world`. The longest resolved phrase wins, with lowest numeric trigger ID resolving equal-length phrase conflicts. One resolved trigger row is selected uniformly among that winning phrase's rows, preserving duplicate weighting and variants. Unresolved references never match, and one message emits at most one automatic intent.

The existing manual typed video endpoint keeps its independent catalog/source authorization and repeated-publication semantics. Video consumption does not grant catalog listing or publication authority. No media download, server playback or media proxy is added. Deployment reachability, request-query logging, browser/provider compatibility and OBS audio require separate operational validation; serving these pages does not establish a public access route.
