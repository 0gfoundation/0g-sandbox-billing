# Build stage
FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o billing ./cmd/billing/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o broker  ./cmd/broker/

# ── sandbox ────────────────────────────────────────────────────────────────────
FROM alpine:3.19 AS sandbox
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /app/billing .
ENTRYPOINT ["./billing"]

# ── broker ─────────────────────────────────────────────────────────────────────
FROM alpine:3.19 AS broker
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /app/broker .
ENTRYPOINT ["./broker"]
