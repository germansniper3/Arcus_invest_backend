#!/bin/sh
set -e

# A Railway (or any Docker) volume mounted at STORAGE_DIR comes up owned by root,
# so the unprivileged app user cannot write to it and uploads fail with a
# permission error. We start as root, ensure the dir exists and is owned by the
# app user, then drop privileges to run the server as that user.
: "${STORAGE_DIR:=/app/storage/uploads}"
mkdir -p "$STORAGE_DIR"
chown -R arcus:arcus "$STORAGE_DIR" 2>/dev/null || echo "entrypoint: warning — could not chown $STORAGE_DIR"

exec su-exec arcus:arcus "$@"
