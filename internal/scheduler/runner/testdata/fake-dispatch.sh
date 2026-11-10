#!/usr/bin/env bash
# Fake dispatch.sh for the scheduler runner tests. The runner invokes it as
#   bash <this> --to <agent> -C <dir> -o <out> --prompt-file <p> ...
# The first line of the prompt file picks the behaviour:
#   ok                 write a passing verdict and exit 0
#   fail               write a failing verdict and exit 7
#   receipt <class>    like fail, plus a bound receipt with that failure_class
#   gate <file>        write <out>.started, wait until <file> exists, then ok
#   hang               write <out>.started and wait until signalled
#   stubborn           like hang, but ignores SIGTERM (only SIGKILL ends it)
#   noverdict          exit 0 without a verdict (collect infers a failure)
#   sleep <n>          write <out>.started, run for n seconds, then ok
#   spawn-child        leave a live descendant (sleep 300, pid in <out>.child), then ok
#   setsid-clear-env   like spawn-child, but the descendant has its own session and no environment
#   fork-storm         write <out>.started, then fork descendants (up to 500) until signalled
# Every invocation appends one line to $FAKE_LOG when it is set, so a test can
# count how many times the executor ran.
out=""; pf=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    --prompt-file) pf="$2"; shift 2 ;;
    *) shift ;;
  esac
done
read -r mode arg < "$pf"
if [ -n "${FAKE_LOG:-}" ]; then
  printf 'run pid=%s out=%s mode=%s dispatch=%s job=%s attempt=%s\n' \
    "$$" "$out" "$mode" "${IC_DISPATCH_ID:-}" "${IC_SCHEDULER_JOB_ID:-}" "${IC_SCHEDULER_ATTEMPT:-}" >> "$FAKE_LOG"
fi
trap 'exit 143' TERM INT
verdict() { printf -- '--- VERDICT ---\nSTATUS: %s\nSUMMARY: %s\n---\n' "$1" "$2" > "$out.verdict"; }
case "$mode" in
  ok) echo "done" > "$out"; verdict pass ok; exit 0 ;;
  fail) echo "x" > "$out"; verdict fail "fake fail"; exit 7 ;;
  receipt)
    echo "x" > "$out"; verdict fail "fake fail"
    printf '{"attempt_id":"%s","failure_class":"%s"}\n' "${IC_DISPATCH_ID:-}" "$arg" > "$out.receipt.json"
    exit 7 ;;
  gate)
    : > "$out.started"
    while [ ! -e "$arg" ]; do sleep 0.02; done
    echo "done" > "$out"; verdict pass ok; exit 0 ;;
  hang)
    : > "$out.started"
    while :; do sleep 0.05; done ;;
  stubborn)
    trap '' TERM INT
    : > "$out.started"
    while :; do sleep 0.05; done ;;
  noverdict) echo "done" > "$out"; exit 0 ;;
  sleep)
    : > "$out.started"
    end=$((SECONDS + arg))
    while [ "$SECONDS" -lt "$end" ]; do sleep 0.1; done
    echo "done" > "$out"; verdict pass ok; exit 0 ;;
  spawn-child)
    sleep 300 >/dev/null 2>&1 </dev/null &
    echo $! > "$out.child"
    : > "$out.started"
    echo "done" > "$out"; verdict pass ok; exit 0 ;;
  setsid-clear-env)
    env -i setsid sleep 300 >/dev/null 2>&1 </dev/null &
    echo $! > "$out.child"
    : > "$out.started"
    echo "done" > "$out"; verdict pass ok; exit 0 ;;
  fork-storm)
    n=0
    while :; do
      ( exec sleep 300 >/dev/null 2>&1 </dev/null ) &
      n=$((n + 1))
      [ "$n" -eq 20 ] && : > "$out.started"
      [ "$n" -lt 500 ] || sleep 0.1
    done ;;
  *) echo "fake-dispatch: unknown mode '$mode'" >&2; exit 64 ;;
esac
