#!/bin/sh
#
# Builds a configuration file from the environment and hands over to
# NFO Updater.
#
# The container is configured through .env, but the program itself reads a
# configuration file and nothing else. This script bridges the two: it takes
# the same config.conf template that ships with every installation, replaces
# the values that were given in the environment, and writes the result to a
# temporary file that lives and dies with the container.
#
# There is deliberately no second template. The file baked into the image is
# the same one the installer writes, so a setting cannot exist in one place
# and be missing from the other.

set -eu

TEMPLATE=/usr/share/nfo_updater/config.conf

# This is where NFO Updater looks by default inside the image: XDG_CONFIG_HOME
# is set to /run in the Dockerfile. Generating the file at exactly that path
# means "docker exec nfo_updater nfo_updater -v" and anything else run by hand
# finds the same configuration the daemon is using, with no --config to
# remember and no chance of a second, empty one being created beside it.
CONFIG=/run/nfo_updater/config.conf

MOVIES_MOUNT=/movies
TVSHOWS_MOUNT=/tvshows

fail() {
    echo ""
    echo "──────────────────────────────────────────────────────────────────"
    echo "  NFO Updater cannot start"
    echo "──────────────────────────────────────────────────────────────────"
    echo "$@"
    echo ""
    echo "Fix it, then apply the change with:"
    echo ""
    echo "  docker compose up -d"
    echo ""
    echo "Editing .env is not enough on its own: a container keeps the"
    echo "environment it was created with, so it has to be recreated."
    echo "──────────────────────────────────────────────────────────────────"
    echo ""
    # Sleeping rather than exiting, on purpose. Exiting would hand control to
    # the restart policy, which would start this same container again with the
    # same environment, fail again, and keep doing so for as long as the host
    # is up. The loop could never succeed, because a new .env only reaches the
    # container when it is recreated. Standing still instead leaves the message
    # above as the last thing in the log, where it can be read.
    #
    # The trap is not decoration. This shell is process 1, and process 1 is not
    # subject to the default signal actions: the kernel delivers only what the
    # process has explicitly asked for. Without a handler, SIGTERM would be
    # discarded, and "docker compose down" would sit through the whole
    # stop_grace_period — five minutes — before resorting to SIGKILL.
    trap 'exit 0' TERM INT
    sleep infinity &
    wait $!
    exit 0
}

# ------------------------------------------------------------------------------
# Media library
#
# The library is mounted at two fixed points rather than at its own paths, so
# that any number of source directories can be attached as subdirectories of
# them. A missing mount point means that category was not attached at all,
# which is allowed as long as the other one was.
# ------------------------------------------------------------------------------
MOVIES=""
TVSHOWS=""
[ -d "$MOVIES_MOUNT" ] && MOVIES=$MOVIES_MOUNT
[ -d "$TVSHOWS_MOUNT" ] && TVSHOWS=$TVSHOWS_MOUNT

if [ -z "$MOVIES" ] && [ -z "$TVSHOWS" ]; then
    fail "  Neither $MOVIES_MOUNT nor $TVSHOWS_MOUNT is mounted, so there is no library
  to work on. Add at least one of them to the volumes section of
  docker-compose.yml."
fi

# A mounted but empty directory is almost always a typo in the host path on
# the left-hand side of the volume: Docker creates whatever is missing, so the
# mount succeeds and the mistake would otherwise show up as a pass that finds
# nothing at all.
warn_if_empty() {
    [ -d "$1" ] || return 0
    [ -z "$(ls -A "$1" 2>/dev/null)" ] || return 0
    echo "warning: $1 is mounted but empty — check the host path in docker-compose.yml" >&2
}
warn_if_empty "$MOVIES_MOUNT"
warn_if_empty "$TVSHOWS_MOUNT"

# ------------------------------------------------------------------------------
# Configuration file
# ------------------------------------------------------------------------------
mkdir -p /data

# Every setting in the template is looked up in the environment. An empty or
# unset variable leaves the template value untouched, which is what makes a
# half-filled .env work and what lets a new release add a setting without
# anyone having to edit anything.
#
# The five settings below are not taken from the environment at all. The three
# paths are fixed by the image, and pointing them anywhere else would put the
# database and the backups inside the container, where the next update discards
# them. The two library paths are decided by what is mounted.
FIXED="DATABASE_PATH LOG_DIR BACKUP_DIR MOVIES_PATH TVSHOWS_PATH"

is_fixed() {
    for f in $FIXED; do
        [ "$1" = "$f" ] && return 0
    done
    return 1
}

mkdir -p "$(dirname "$CONFIG")"
: > "$CONFIG"
chmod 600 "$CONFIG"

while IFS= read -r line; do
    key=$(echo "$line" | sed -n 's/^\([A-Z_][A-Z0-9_]*\)=.*/\1/p')

    if [ -z "$key" ] || is_fixed "$key"; then
        printf '%s\n' "$line" >> "$CONFIG"
        continue
    fi

    # eval, because the variable name is only known at run time. The name has
    # already been matched against [A-Z_][A-Z0-9_]* above, so nothing else can
    # get in here.
    eval "value=\${$key:-}"

    if [ -n "$value" ]; then
        printf '%s=%s\n' "$key" "$value" >> "$CONFIG"
    else
        printf '%s\n' "$line" >> "$CONFIG"
    fi
done < "$TEMPLATE"

# The fixed five, appended at the end. A later assignment wins over an earlier
# one, so these override whatever the template said without the template having
# to be edited.
{
    echo ""
    echo "# --- set by the container, not configurable ---"
    echo "DATABASE_PATH=/data/database.db"
    echo "LOG_DIR=/data/logs"
    echo "BACKUP_DIR=/data/backups"
    [ -n "$MOVIES" ] && echo "MOVIES_PATH=$MOVIES"
    [ -n "$TVSHOWS" ] && echo "TVSHOWS_PATH=$TVSHOWS"
} >> "$CONFIG"

# ------------------------------------------------------------------------------
# Check before starting
#
# --validate reads the file and reports what is wrong with it, without touching
# the network or the library. Doing this here, rather than letting the daemon
# fail on its own, is what makes a bad configuration stop the container instead
# of putting it into a restart loop.
# ------------------------------------------------------------------------------
if ! nfo_updater --config "$CONFIG" --validate > /dev/null 2>&1; then
    echo ""
    nfo_updater --config "$CONFIG" --validate 2>&1 || true
    fail "  The settings above came from .env."
fi

# exec, so that NFO Updater becomes process 1 and receives signals directly:
# SIGTERM to stop, SIGHUP to re-read, SIGUSR1 to start a pass now. A wrapper
# left in between would swallow all three.
exec nfo_updater --config "$CONFIG" "$@"
