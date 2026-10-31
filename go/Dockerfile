# One image for every Go process (slice 3 decision 53): each binary under cmd/ is built and named by
# its process (api, relay, deposit-worker, submitter), and compose picks the command (step 8).

# Build stage: the Go toolchain go.mod pins (go1.27.3), on the same Alpine as the Node image.
FROM golang:1.27.3-alpine3.24 AS build
# Use this image's Go, never download another: a go.mod asking for a newer toolchain should fail
# the build, not fetch one.
ENV GOTOOLCHAIN=local
WORKDIR /src
# The modules first, as their own layer: downloaded again only when go.mod or go.sum change, not on
# every source change.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO_ENABLED=0: pure Go, statically linked, so the binaries run on an image with no C library.
# -trimpath: no local file paths in the binaries (reproducible, and nothing about the build host).
# -o /out/ with ./cmd/...: every command, each named after its directory.
RUN CGO_ENABLED=0 go build -trimpath -o /out/ ./cmd/...

# Final stage: distroless static, which holds CA certificates, time zone data and a passwd entry
# for nonroot, and nothing else (no shell, no package manager). Pinned by digest: the nonroot tag
# moves with every rebuild of the image.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
COPY --from=build /out/ /usr/local/bin/
# The image's unprivileged user (uid 65532) rather than root.
USER nonroot
# No default command: compose names the binary (command: ["api"]), as the Node image's services
# name their script. Each process handles SIGTERM itself (it is PID 1), so docker compose stop
# shuts it down cleanly.
