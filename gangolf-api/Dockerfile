FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gangolf-api .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 gangolf
USER gangolf
COPY --from=build /out/gangolf-api /usr/local/bin/gangolf-api
ENTRYPOINT ["/usr/local/bin/gangolf-api"]
