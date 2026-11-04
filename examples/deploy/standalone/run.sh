#!/bin/sh
# Runs the Manager in one container, no compose (default mode: Runner processes run inside the container).
#
# Usage:
#   sh run.sh                       # listens on 127.0.0.1:8080, no auth
#   PORT=0.0.0.0:8080 BASIC_AUTH_PASSWORD=xxx sh run.sh
#   WITH_DOCKER=0 sh run.sh         # Jobs don't need docker; host socket not mounted
#
# Variables:
#   IMAGE                 Manager image, default ghcr.io/sharkpaw/runner-deck:v2.1.0
#   PORT                  Host listen address:port, default 127.0.0.1:8080
#   DATA_DIR              Parent directory of config/ and runners/, default current directory
#   BASIC_AUTH_USER       default admin
#   BASIC_AUTH_PASSWORD   Leave empty to disable auth (GET /health is always unauthenticated)
#   WITH_DOCKER           1 (default) mounts the host docker.sock for Jobs; 0 does not mount it.
#                         Mounting it is equivalent to handing host root to Jobs: in this mode Jobs and the Manager
#                         share one container and user, so any Job can use it to control host Docker. Set 0 if not needed.
#
# The --stop-timeout 30 below is not optional: in default mode the Runner processes live in the Manager container,
# and on SIGTERM the Manager stops them before exiting, while docker stop only waits 10 seconds by default.

set -e

IMAGE="${IMAGE:-ghcr.io/sharkpaw/runner-deck:v2.1.0}"
PORT="${PORT:-127.0.0.1:8080}"
DATA_DIR="${DATA_DIR:-$(pwd)}"
BASIC_AUTH_USER="${BASIC_AUTH_USER:-admin}"
BASIC_AUTH_PASSWORD="${BASIC_AUTH_PASSWORD:-}"
WITH_DOCKER="${WITH_DOCKER:-1}"
NAME="${NAME:-runner-manager}"

# The Manager runs as UID 1001, so the mounted directories must be writable by it.
# Create the directories first, then chown: in the reverse order, files placed in them later stay owned by root,
# 写配置时会报「保存配置失败: open /app/config/config.yaml: permission denied」。
mkdir -p "$DATA_DIR/config" "$DATA_DIR/runners"
if [ "$(id -u)" = "0" ]; then
    chown -R 1001:1001 "$DATA_DIR/config" "$DATA_DIR/runners"
else
    echo "提示: 非 root 运行，请自行确认 $DATA_DIR/config 与 $DATA_DIR/runners 对 UID 1001 可写"
fi

set -- run -d --name "$NAME" \
    -p "$PORT:8080" \
    -v "$DATA_DIR/config:/app/config" \
    -v "$DATA_DIR/runners:/app/runners" \
    -e RUNNERS_BASE_PATH=/app/runners \
    -e BASIC_AUTH_USER="$BASIC_AUTH_USER" \
    -e BASIC_AUTH_PASSWORD="$BASIC_AUTH_PASSWORD" \
    --restart unless-stopped \
    --stop-timeout 30

if [ "$WITH_DOCKER" = "1" ]; then
    # Inside the container the user is UID 1001, which must be in the socket's group, otherwise docker in Jobs fails with permission denied.
    // Anyone who gets into this group effectively gets host root — the Job and Manager run as the same user, so nothing stops it.
    DOCKER_GID="${DOCKER_GID:-$(getent group docker 2>/dev/null | cut -d: -f3 || true)}"
    [ -n "$DOCKER_GID" ] || DOCKER_GID=999
    set -- "$@" -v /var/run/docker.sock:/var/run/docker.sock --group-add "$DOCKER_GID"
fi

docker rm -f "$NAME" >/dev/null 2>&1 || true
docker "$@" "$IMAGE"

echo "Started $NAME: http://$PORT"
echo "自检日志：docker logs $NAME | grep 自检"
