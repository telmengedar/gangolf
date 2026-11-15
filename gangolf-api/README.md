# Gangolf API

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


## Live events

`GET /api/v3/events` opens a live SSE subscription. Use the existing bearer API key or JWT in the Authorization header. Query credentials are rejected, including when a bearer is present. Fetch supports this header; native `EventSource` does not. Consume through an authorized same-origin route. The current deployment listens on host loopback; this source change does not configure browser reachability, CORS, TLS, or proxy buffering.

```sh
curl --no-buffer --get "$GANGOLF_API/api/v3/events" \
  --header "Authorization: Bearer $GANGOLF_TOKEN" \
  --header 'Accept: text/event-stream' \
  --data-urlencode 'types=message.created,video.triggered' \
  --data-urlencode 'service=Twitch' \
  --data-urlencode 'channel=101'
```

`types` is required: one or both of `message.created` and `video.triggered`, comma-separated, without duplicates. Optional `service` and `channel` are exact, case-sensitive filters; `channel` requires `service`. Types are ORed, service/channel are ANDed. Omit service/channel to receive all currently readable events. Unknown, repeated, empty, malformed, or unsupported parameters return 400 `invalid_filter`; decoded service/channel have a 256-character limit and the query a 4096-byte limit. Filters never grant permission.

The first flushed frame is:

```text
event: stream.ready
data: {"schemaVersion":1,"replay":false,"resyncRequired":true}

```

Data frames have `event: message.created` or `event: video.triggered` and one compact JSON `data:` line. Their envelope is `{schemaVersion:1,id,type,time,service,channel,channelId,data}`. `id` is an unpredictable process epoch plus a process-local sequence. It is useful for diagnostics/deduplication, never a durable cursor; sequence gaps are normal. `time` is server publication UTC time. `channelId` is a v3 channel ID or null for legacy v2 messages. No SSE `id:` field is sent.

Message data is either `{resourceVersion:3,message:<existing v3 message resource>}` or `{resourceVersion:2,batchId,index,message:<existing v2 message>}`. Legacy `index` is the zero-based batch position. Actual accepted chunks, timestamps, source and delivery fields are preserved. Events cover successful local v3 creation, legacy batches, new Twitch inbound messages, and the first confirmed outbound reservation. Outbound creation honestly has `delivery.state:"unknown"` because the event precedes sending. Retries/replays, terminal delivery changes, PATCH, DELETE, history reads and imports do not emit a second creation event.

Each data frame and every 15-second heartbeat reloads authorization. API keys retain their own effective permissions even if their user is admin. Existing message-read rules apply: Twitch messages require the current tenant/broadcaster/channel association or effective admin; non-Twitch messages remain shared among authorized hosts. Disabled Twitch profiles retain their existing read semantics. Video subscriptions additionally require available current media storage and its configured tenant or effective admin. A nonadmin never receives video intents derived from shared non-Twitch messages. Revocation, expiry, unavailable auth/media storage, disconnect, overflow, ambiguous message creation or shutdown ends the stream; idle revocation is detected on the next heartbeat plus its bounded 10-second authorization operation. Message-only streams do not depend on media availability.

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

The caller must be the configured media tenant or effective admin, and the source must be a visible nondeleted v3 message with validated relations. Nonadmins additionally need a Twitch source belonging to the catalog tenant through its current profile association; shared non-Twitch records have no tenant attribution. The current trigger must resolve to an existing clip. The server derives routing and `{messageId,trigger,clip}` from current stored DTOs, then returns 200 with the emitted envelope, even with no subscribers. There is no durable item/Location or event GET history, no storage writes and no automatic text-match requirement. A repeated POST creates a fresh intent. An ambiguous response must not be blindly retried because duplicate intents are possible.

Missing message/trigger returns 404 `not_found`, unauthorized source 403 `forbidden`, unresolved clip 409 `unresolved_trigger`, and absent/unavailable media 503 `media_unavailable`. Existing bearer authentication failures apply. Other methods return 405 `Allow: POST`. Reuse the ordinary clips/triggers API to manage the catalog; archives are never consulted by event production.
