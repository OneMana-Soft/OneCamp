# Two stages: the Go toolchain builds, a bare Alpine runs.
#
# WHY. The single-stage image shipped the entire Go toolchain, source tree and
# module cache to production — 3.49GB for a binary of a few tens of megabytes.
# Every managed workspace is its own server, so that weight is paid per tenant:
# on the pull, on the disk, and again on every rebuild.
#
# It is also a smaller attack surface. A compiler, a package manager and the full
# source of the application are things an intruder can use; none of them are things
# the service needs in order to run.

# ---- build ---------------------------------------------------------------
FROM golang:1.26-alpine AS build

WORKDIR /app

# DEPENDENCIES BEFORE SOURCE. `COPY . .` first meant every code change invalidated
# the layer below it, so `go mod download` re-fetched the whole module graph on
# every build — slow, and it wrote a fresh set of cache layers each time that
# nothing would ever reuse. Copying go.mod/go.sum on their own means the download
# is reused until the dependencies themselves change.
COPY go.mod go.sum ./

RUN go mod download

COPY . .

# CGO_ENABLED=0 makes the binary static, which is what lets it run on an image
# that has no Go runtime and no libc it was linked against. Nothing here needs
# cgo — there is no sqlite or librdkafka in go.sum — so this costs nothing.
#
# -s -w drop the symbol and DWARF tables. They are only useful for debugging a
# core dump, which is not how this service is diagnosed, and they are a large
# fraction of the binary.
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o go-one-camp cmd/server/*.go

# ONLY THE BINARY GOES TO THE RUNTIME.
#
# Deliberately not firebase-cred.json, which used to arrive here via `COPY . .`.
# It is a service-account private key and it was committed to this repository,
# which means it shipped inside the archive every customer downloads and was baked
# into every image. .dockerignore now keeps it out of the build context entirely.
#
# Push notifications stay optional and the code already handles the file being
# absent. A deployment that wants them mounts the key in and points
# FIREBASE_CRED_PATH at where it was mounted — which is how a credential should
# reach a container in any case, rather than being compiled into an image that
# then has to be treated as a secret itself.

# ---- runtime -------------------------------------------------------------
FROM alpine:3.22

# ca-certificates: alpine ships none, and without them every outbound HTTPS call
#   fails certificate verification — AI providers, email, object storage.
# curl: the compose healthcheck runs `curl -f http://localhost:3000/health`, so
#   the container is marked unhealthy forever without it.
# tzdata: Go reads /usr/share/zoneinfo unless the tzdata package is imported, and
#   a workspace schedules things in the user's timezone.
RUN apk add --no-cache ca-certificates curl tzdata

WORKDIR /app

COPY --from=build /app/go-one-camp ./

EXPOSE 3000

CMD [ "./go-one-camp" ]
