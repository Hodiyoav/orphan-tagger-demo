FROM golang:1.25 AS builder
WORKDIR /app
COPY watcher/go.mod watcher/go.sum ./
RUN go mod download
COPY watcher/ .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /watcher .

FROM alpine:3.20
COPY --from=builder /watcher /watcher
USER 65534:65534
ENTRYPOINT ["/watcher"]
