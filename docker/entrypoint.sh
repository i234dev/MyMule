#!/bin/sh
# Container entrypoint: prepares /config and /downloads for the user
# PUID:PGID (1000:1000 by default) and runs MyMule as that user, never as
# root. Started with --user instead, it runs as that user directly.
set -eu

uid=${PUID:-1000}
gid=${PGID:-1000}

as_user() {
	if [ "$(id -u)" = 0 ]; then
		exec su-exec "$uid:$gid" "$@"
	fi
	exec "$@"
}

if [ "${1:-}" = healthcheck ]; then
	as_user mymule status >/dev/null
fi

if [ "$(id -u)" = 0 ]; then
	case "$uid:$gid" in
	*[!0-9:]* | :* | *:) echo "PUID and PGID must be numbers (got '$uid:$gid')" >&2; exit 1 ;;
	esac
	if [ "$uid" = 0 ] || [ "$gid" = 0 ]; then
		echo "MyMule does not run as root: set PUID and PGID to a normal user (e.g. 1000)" >&2
		exit 1
	fi
	mkdir -p "$MYMULE_HOME" "$MYMULE_DOWNLOAD_DIR" "$MYMULE_TEMP_DIR"
	# The data folder is small and only MyMule's: all of it. The download
	# folders may be a big shared library: only the folders themselves.
	# Some mounts (Windows and macOS folders, NAS shares) don't allow
	# changing owners; MyMule then says which folder it cannot write.
	chown -R "$uid:$gid" "$MYMULE_HOME" 2>/dev/null ||
		echo "note: cannot change the owner of $MYMULE_HOME to $uid:$gid" >&2
	chown "$uid:$gid" "$MYMULE_DOWNLOAD_DIR" "$MYMULE_TEMP_DIR" 2>/dev/null ||
		echo "note: cannot change the owner of $MYMULE_DOWNLOAD_DIR to $uid:$gid" >&2
fi

as_user "$@"
