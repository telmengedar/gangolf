# ai-core

Gangolf's chat brain: a small Go service that polls the online message API, keeps a short per-channel window of the conversation, asks the processor to answer new lines and posts the answer back. Gangolf may also decide he has nothing to add, in which case nothing is posted.

## Configuration

All configuration is environment variables.

| Variable | Required | Meaning |
|---|---|---|
| `GANGOLF_API_KEY` | yes | message API key; never put it in the image or the repo |
| `GANGOLF_CHANNELS` | yes | comma-separated `service/channel` list Gangolf answers in |
| `GANGOLF_ROUTES` | no | comma-separated `service/channel=targets` entries; unset means every channel is answered in itself (see Routing) |
| `GANGOLF_MIN_WORDS` | no | comma-separated `service/channel=n` entries; a line from that source with fewer than `n` words never starts a run (see Word threshold); unset means no threshold |
| `GANGOLF_API_URL` | no | message API base, default `https://gangolf.mamgo.io/api` |
| `GANGOLF_BOT_NAME` | no | Gangolf's speaker name, default `GangolfBot` |
| `PROCESSOR_URL` | no | processor base, default `http://gangolf:8080` |
| `PROCESSOR_SUBJECT` | no | node the processor anchors on, default `1090` |
| `PROCESSOR_MEMORY_CONFIG` | no | positive config node ID for private memory review; unset or blank disables it; requires a dedicated Processor `note.mode=decision` policy |
| `PROCESSOR_CONFIG` | no | id of the `processor-config` node sent with every run, default `17607` (Gangolf's config) |

Private memory review runs after a successful public post, using frozen speaker statements with generated assistant replies excluded from its evidence. Its response never reaches chat. One worker has one waiting slot; new jobs are dropped when busy, and each review has a 60-second timeout with no retry. Shutdown cancels active review and discards waiting work. A timeout leaves remote persistence unknown. Review shares model capacity with chat; enabling it requires a compatible Processor and measured memory quality and latency.

## Routing

By default a line is answered in the channel it was written in. `GANGOLF_ROUTES` changes that per source channel: an entry `service/channel=targets` copies the source's lines into the conversation of each target, and the answer is posted to the target. `targets` is `*` (every channel that answers its own lines) or one or more `service/channel` joined by `|`.

```
GANGOLF_CHANNELS=Twitch/rugenforth,localhost/host
GANGOLF_ROUTES=localhost/host=*
```

- Every source and every target must be listed in `GANGOLF_CHANNELS`, and a source may appear once.
- A channel routed away from itself has no conversation of its own and is never posted to, so a meta channel such as `localhost/host` needs a route. Listing it in `GANGOLF_CHANNELS` without one answers it into itself.
- A target must answer its own lines: it cannot itself be routed elsewhere. `*` is only allowed when at least one channel does.
- With several targets each channel's conversation answers on its own, so the same line can get a different answer, or silence, in each.
- A speech fragment can start a run before the next fragment of the same sentence arrives. The continuation is then read as context only.
- A startup error names the variable and the offending entry and exits with code 1.

## Word threshold

`GANGOLF_MIN_WORDS=localhost/host=4` stops lines from that source with fewer than 4 words from starting a run. The threshold belongs to the source: it follows the line into every target, and a channel without an entry has none, so a one-word Twitch line still starts a run.

- A word is a whitespace-separated token with at least one letter or digit: `uh...` is one word and `...` is none.
- A short line is not dropped. It joins the conversation as a pending line, so a long line written within the settle time carries it into the run in order, and a short line that starts nothing shows up in the history of the next run.
- A batch runs only if at least one of its lines can start a run and is not written to someone else (the addressee gate); it then runs with all its lines, short ones included. A batch made only of short lines logs `outcome=too-short`. If every line that can start a run is written to someone else, it logs `outcome=addressed-elsewhere`, also when short lines are in the batch. Neither makes a run, and the lines stay as context for the next run.
- The minimum is at least 1; `1` only stops lines without any word.

## Build and run

```
docker build -t gangolf-ai-core ai-core
docker run -d --restart unless-stopped --name ai-core --add-host gangolf:host-gateway --env-file <env file outside any checkout> gangolf-ai-core
```
