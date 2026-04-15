FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY verify.go .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o verify .

FROM alpine:3.20
COPY --from=builder /app/verify /verify
EXPOSE 8080
ENTRYPOINT ["/verify"]
