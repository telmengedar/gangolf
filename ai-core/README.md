# ai-core

Gangolf's chat brain: a small Go service that polls the online message API, keeps a short per-channel window of the conversation, asks the processor to answer new lines and posts the answer back. Gangolf may also decide he has nothing to add, in which case nothing is posted.

## Configuration

All configuration is environment variables.

| Variable | Required | Meaning |
|---|---|---|
| `GANGOLF_API_KEY` | yes | message API key; never put it in the image or the repo |
| `GANGOLF_CHANNELS` | yes | comma-separated `service/channel` list Gangolf answers in |
| `GANGOLF_API_URL` | no | message API base, default `https://gangolf.mamgo.io/api` |
| `GANGOLF_BOT_NAME` | no | Gangolf's speaker name, default `GangolfBot` |
| `PROCESSOR_URL` | no | processor base, default `http://gangolf:8080` |
| `PROCESSOR_SUBJECT` | no | node the processor anchors on, default `1090` |

## Build and run

```
docker build -t gangolf-ai-core ai-core
docker run -d --restart unless-stopped --name ai-core --add-host gangolf:host-gateway --env-file <env file outside any checkout> gangolf-ai-core
```
