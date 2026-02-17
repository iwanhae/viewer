#!/bin/sh
set -eu

RECOMMENDER_LISTEN_ADDR="${RECOMMENDER_LISTEN_ADDR:-0.0.0.0:18081}"
export RECOMMENDER_LISTEN_ADDR

echo "entrypoint: starting recommender on ${RECOMMENDER_LISTEN_ADDR}" >&2
/app/recommender &
RECOMMENDER_PID=$!

echo "entrypoint: starting viewer on port ${PORT:-8080}" >&2
/app/viewer &
VIEWER_PID=$!

RECOMMENDER_EXITED=0
shutdown_children() {
    # Forward stop signals to both services; ignore already-exited children.
    kill -TERM "$RECOMMENDER_PID" "$VIEWER_PID" 2>/dev/null || true
}

on_signal() {
    shutdown_children
    wait "$RECOMMENDER_PID" 2>/dev/null || true
    wait "$VIEWER_PID" 2>/dev/null || true
    exit 143
}

trap 'on_signal' INT TERM

while :; do
    if ! kill -0 "$RECOMMENDER_PID" 2>/dev/null; then
        if [ "$RECOMMENDER_EXITED" -eq 0 ]; then
            status=0
            wait "$RECOMMENDER_PID" 2>/dev/null || status=$?
            echo "entrypoint: recommender exited with status ${status}, continuing viewer in degraded mode" >&2
            RECOMMENDER_EXITED=1
        fi
    elif [ "$RECOMMENDER_EXITED" -ne 0 ]; then
        # recommender recovered/restarted externally.
        RECOMMENDER_EXITED=0
    fi

    if ! kill -0 "$VIEWER_PID" 2>/dev/null; then
        status=0
        wait "$VIEWER_PID" || status=$?
        echo "entrypoint: viewer exited with status ${status}, stopping recommender" >&2
        kill -TERM "$RECOMMENDER_PID" 2>/dev/null || true
        wait "$RECOMMENDER_PID" 2>/dev/null || true
        exit "$status"
    fi

    sleep 1
done
