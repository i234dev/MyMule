#!/bin/sh
# `mymule` in the container. Run as root (docker exec, the health check),
# it runs MyMule as PUID:PGID instead, so the files in /config never end
# up owned by root, where the daemon could no longer write them.
set -eu
if [ "$(id -u)" = 0 ]; then
	uid=${PUID:-1000}
	gid=${PGID:-1000}
	case "$uid:$gid" in
	*[!0-9:]* | :* | *: | 0:* | *:0)
		echo "MyMule does not run as root: set PUID and PGID to a normal user (e.g. 1000)" >&2
		exit 1
		;;
	esac
	exec su-exec "$uid:$gid" /usr/local/bin/mymule.bin "$@"
fi
exec /usr/local/bin/mymule.bin "$@"
