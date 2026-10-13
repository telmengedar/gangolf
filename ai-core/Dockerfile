FROM golang:1.27 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ai-core .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 aicore
USER aicore
COPY --from=build /out/ai-core /usr/local/bin/ai-core
ENTRYPOINT ["/usr/local/bin/ai-core"]
