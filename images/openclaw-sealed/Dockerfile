# OpenClaw Sealed Sandbox
#
# Layers the sealed-container bootstrap binary on top of the openclaw image.
# Bootstrap reads the agent config from encrypted i-data, decrypts it,
# generates ~/.openclaw/openclaw.json, and spawns `openclaw gateway run`.
# Then the same process serves :8080 as a signed reverse-proxy in front of
# openclaw on :3284.
#
# Build context must be the repo root (bootstrap source is in test/sealed/):
#   docker build -t openclaw-sealed:0.1.0 -f images/openclaw-sealed/Dockerfile .

# Stage 1 — 0g-storage-client (used by bootstrap to download intelligent data).
FROM golang:1.24-alpine AS storage-builder
RUN apk add --no-cache git
WORKDIR /src
RUN git clone --depth 1 https://github.com/0gfoundation/0g-storage-client.git
WORKDIR /src/0g-storage-client
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/0g-storage-client .

# Stage 2 — bootstrap binary from test/sealed/.
FROM golang:1.24-alpine AS bootstrap-builder
WORKDIR /app
COPY test/sealed/go.mod test/sealed/go.sum ./
RUN go mod download
COPY test/sealed/*.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o bootstrap .

# Stage 3 — runtime: openclaw image already has node + openclaw + nvm PATH set.
FROM eliza-registry-vpc.ap-southeast-1.cr.aliyuncs.com/eliza/0g-sandbox:openclaw-1.0.0

COPY --from=storage-builder /out/0g-storage-client /usr/local/bin/0g-storage-client
COPY --from=bootstrap-builder /app/bootstrap /bootstrap

# Bootstrap launches openclaw itself once it has decrypted the config — no
# need for the original start.sh.
ENTRYPOINT ["/bootstrap"]
EXPOSE 8080
