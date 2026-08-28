#!/bin/sh
# Container entrypoint. Without SIDECAR_BACKUP_BUCKET it execs the sidecar
# directly, exactly as before. With it, Litestream first restores the
# database from the replica when the local file is missing (a fresh disk, or
# a rebuilt service), then runs the sidecar as its child and streams every
# WAL change to the replica for as long as the sidecar runs. Litestream
# forwards SIGTERM to the child and exits when it does, so Render's stop
# signal still reaches the server's graceful shutdown.
#
# Sidecar flags pass straight through: `entrypoint.sh --refresh 30m`.
set -eu

if [ -z "${SIDECAR_BACKUP_BUCKET:-}" ]; then
  exec sidecar "$@"
fi

: "${SIDECAR_DB:?SIDECAR_DB must be set when backups are enabled}"
export SIDECAR_BACKUP_PATH="${SIDECAR_BACKUP_PATH:-sidecar}"
export SIDECAR_BACKUP_REGION="${SIDECAR_BACKUP_REGION:-auto}"
export SIDECAR_BACKUP_RETENTION="${SIDECAR_BACKUP_RETENTION:-168h}"
config=/etc/litestream.yml

# -if-db-not-exists: a database already on the disk wins over the replica;
# -if-replica-exists: a brand-new deployment with nothing replicated yet
# starts empty instead of failing to boot. The timeout matters: Litestream
# retries an unreachable bucket quietly and indefinitely, and a boot that
# hangs looks the same as one that is working. Failing instead makes the
# outage a visible crash loop -- deliberately not a fallback to an empty
# database, which would fork history from the replica.
restore_timeout="${SIDECAR_BACKUP_RESTORE_TIMEOUT:-120}"
timeout "$restore_timeout" litestream restore -config "$config" -if-db-not-exists -if-replica-exists "$SIDECAR_DB" || {
  rc=$?
  # GNU timeout reports 124; busybox's (this image) passes through the
  # child's SIGTERM status, 143.
  if [ "$rc" -eq 124 ] || [ "$rc" -eq 143 ]; then
    echo "litestream restore timed out after ${restore_timeout}s: bucket unreachable or credentials wrong?" >&2
  fi
  exit "$rc"
}
if [ ! -f "$SIDECAR_DB" ]; then
  # -if-replica-exists cannot tell a first boot from a wrong bucket or
  # path: both look like "no replica". Say so loudly; the first
  # replicated transaction below starts a new history at this path.
  echo "WARNING: no replica found at ${SIDECAR_BACKUP_BUCKET}/${SIDECAR_BACKUP_PATH}; starting with an empty database" >&2
fi
# Litestream re-splits the -exec string with shell word rules, so a sidecar
# flag value containing whitespace would be broken apart; none does today.
exec litestream replicate -config "$config" -exec "sidecar $*"
