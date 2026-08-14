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

# The generated configuration is written to the path NFO Updater looks at by
# default, so that anything run with "docker exec" sees the same settings as
# the daemon without being given --config.
ENV XDG_CONFIG_HOME=/run

COPY --from=build /out/nfo_updater /usr/local/bin/nfo_updater
COPY --from=build /src/internal/config/config.conf /usr/share/nfo_updater/config.conf
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

# The template comes straight out of the source tree, so it cannot drift from
# the one a normal installation gets. The entrypoint fills it in from the
# environment on every start; the result lives in /run and is never written to
# a volume, which is why an edited .env can never be shadowed by a stale file.

# /movies and /tvshows are deliberately NOT created here. Their absence is how
# the entrypoint tells that a category was not mounted at all.

# No VOLUME declarations either: they would create anonymous volumes for anyone
# who forgets a bind mount, and the data would then quietly disappear with the
# container instead of failing visibly.

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["-d"]
