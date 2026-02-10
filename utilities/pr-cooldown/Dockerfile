# Build stage
FROM golang:1.22-alpine AS builder

RUN apk add --no-cache gcc musl-dev

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /pr-cooldown ./cmd/server/

# Runtime stage
FROM alpine:3.20

RUN apk add --no-cache ca-certificates

RUN adduser -D -u 1000 appuser

COPY --from=builder /pr-cooldown /usr/local/bin/pr-cooldown

RUN mkdir -p /data && chown appuser:appuser /data

USER appuser

VOLUME /data

EXPOSE 8080

ENTRYPOINT ["pr-cooldown"]
CMD ["--port", "8080", "--db-path", "/data/cooldown.db"]
