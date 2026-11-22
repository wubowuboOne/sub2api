#!/bin/sh
set -eu

attempt=1
max_attempts=3

while :; do
  if go mod download "$@"; then
    exit 0
  else
    status=$?
  fi

  if [ "$attempt" -ge "$max_attempts" ]; then
    printf 'Go module download failed after %s attempts\n' "$max_attempts" >&2
    exit "$status"
  fi

  delay=$((attempt * 2))
  printf 'Go module download failed (attempt %s/%s); retrying in %ss\n' "$attempt" "$max_attempts" "$delay" >&2
  sleep "$delay"
  attempt=$((attempt + 1))
done
