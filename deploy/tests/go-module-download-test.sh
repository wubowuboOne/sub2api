#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
test_dir=$(mktemp -d "${TMPDIR:-/tmp}/sub2api-module-download.XXXXXX")
trap 'rm -rf "$test_dir"' EXIT HUP INT TERM

run_case() {
  name=$1
  succeed_after=$2
  want_status=$3
  want_calls=$4
  want_sleeps=$5
  calls_file="$test_dir/$name.calls"
  sleeps_file="$test_dir/$name.sleeps"
  : > "$calls_file"
  : > "$sleeps_file"

  actual_status=0
  (
    calls=0
    go() {
      calls=$((calls + 1))
      printf '%s\n' "$*" >> "$calls_file"
      [ "$#" -eq 3 ] && [ "$1" = mod ] && [ "$2" = download ] &&
        [ "$3" = fixture.invalid/module@v1.0.0 ] || exit 90
      [ "$GOPROXY" = 'https://proxy.invalid|direct' ] || exit 91
      [ "$GOSUMDB" = sum.golang.google.cn ] || exit 92
      if [ "$calls" -ge "$succeed_after" ]; then
        return 0
      fi
      return 37
    }
    sleep() {
      printf '%s\n' "$1" >> "$sleeps_file"
    }
    export GOPROXY='https://proxy.invalid|direct'
    export GOSUMDB=sum.golang.google.cn
    set -- fixture.invalid/module@v1.0.0
    . "$repo_root/backend/scripts/download-modules.sh"
  ) > "$test_dir/$name.log" 2>&1 || actual_status=$?

  [ "$actual_status" -eq "$want_status" ] || {
    printf '%s: expected status %s, got %s\n' "$name" "$want_status" "$actual_status" >&2
    exit 1
  }
  [ "$(wc -l < "$calls_file" | tr -d ' ')" -eq "$want_calls" ]
  [ "$(tr '\n' ',' < "$sleeps_file")" = "$want_sleeps" ]
}

run_case immediate-success 1 0 1 ''
run_case second-attempt 2 0 2 '2,'
run_case third-attempt 3 0 3 '2,4,'
run_case exhausted 99 37 3 '2,4,'

for dockerfile in Dockerfile deploy/Dockerfile backend/Dockerfile; do
  grep -Fq 'RUN sh ./scripts/download-modules.sh' "$repo_root/$dockerfile" ||
    grep -Fq '    sh ./scripts/download-modules.sh' "$repo_root/$dockerfile"
  grep -Eq '^COPY (backend/)?scripts/download-modules.sh ./scripts/download-modules.sh$' "$repo_root/$dockerfile"
  if grep -Eq '^RUN go mod download$|^    go mod download$' "$repo_root/$dockerfile"; then
    printf '%s bypasses the bounded module download helper\n' "$dockerfile" >&2
    exit 1
  fi
done

for dockerfile in Dockerfile deploy/Dockerfile; do
  grep -Fxq 'ARG GOPROXY=https://goproxy.cn|https://proxy.golang.org|direct' "$repo_root/$dockerfile"
done
grep -Fq '"GOPROXY=${GOPROXY:-https://goproxy.cn|https://proxy.golang.org|direct}"' "$repo_root/deploy/build_image.sh"

printf 'Go module download retry and Docker wiring tests passed\n'
