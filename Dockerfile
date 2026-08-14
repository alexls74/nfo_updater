# syntax=docker/dockerfile:1

# ------------------------------------------------------------------------------
# Build stage
# ------------------------------------------------------------------------------
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies are copied on their own so that the module download is cached
# and only repeated when go.mod or go.sum actually change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Both are supplied by the Makefile. The defaults match the ones compiled into
# internal/version, so a plain "docker build" is visibly an unofficial build
# rather than a release pretending to be one.
ARG VERSION=dev
ARG BUILD_DATE=unknown

# CGO is off: the SQLite driver is pure Go, so the result is a static binary
# that needs nothing from the base image but certificates and time zones.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
        -trimpath \
        -ldflags="-s -w \
            -X nfo_updater/internal/version.Version=${VERSION} \
            -X nfo_updater/internal/version.BuildDate=${BUILD_DATE}" \
        -o /out/nfo_updater ./cmd/nfo_updater

# ------------------------------------------------------------------------------
# Runtime stage
# ------------------------------------------------------------------------------
FROM alpine:3.22

LABEL org.opencontainers.image.source="https://github.com/alexls74/nfo_updater"
LABEL org.opencontainers.image.description="Updates ratings in Kodi NFO files"
LABEL org.opencontainers.image.licenses="MIT"

# ca-certificates: the rating services and the media servers are reached over
#   HTTPS, and without the trust store every request fails.
# tzdata: the schedule is a cron expression evaluated in local time, so the
#   zone named by TZ has to be resolvable.
RUN apk add --no-cache ca-certificates tzdata

COPY --from=build /out/nfo_updater /usr/local/bin/nfo_updater

# Where the configuration and the data live inside the container.
#
# NFO Updater appends its own name to an XDG directory, so these roots put the
# configuration at /etc/nfo_updater/config.conf and the database, the logs and
# the backups under /var/lib/nfo_updater. docker-compose.yml mounts host
# directories onto exactly those two paths.
ENV XDG_CONFIG_HOME=/etc \
    XDG_DATA_HOME=/var/lib

# Tells the setup wizard that it is running inside this image: the questions
# about systemd and about where to keep the data have no answer here. It is
# not configuration and overrides nothing in config.conf.
ENV NFO_UPDATER_CONTAINER=1

# No VOLUME declarations on purpose: they would create anonymous volumes for
# anyone who forgets a bind mount, and the data would then quietly disappear
# with the container instead of failing visibly.

# The binary is the entrypoint, so "docker compose run --rm nfo_updater --setup"
# replaces only the -d below and still reaches the program's own flags.
ENTRYPOINT ["/usr/local/bin/nfo_updater"]
CMD ["-d"]
