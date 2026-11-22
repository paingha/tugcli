# Helpers for the end-to-end suites, sourced by scripts/e2e-kind.sh after it
# has made $work. They run on bash 3.2, as macOS ships it.

# expect_exit writes a command's stdout and stderr to these files.
out="$work/out"
err="$work/err"

# fail reports a failed check, prints each file named after the message, and
# ends the test.
fail() {
  echo "FAIL: $1" >&2
  shift
  local f
  for f in "$@"; do
    echo "--- $f" >&2
    cat "$f" >&2 || true
  done
  exit 1
}

# expect_exit runs a command with its stdout in $out and its stderr in $err,
# and fails unless it exits with the code given first.
expect_exit() {
  local want=$1 got=0
  shift
  "$@" > "$out" 2> "$err" || got=$?
  if [ "$got" != "$want" ]; then
    fail "$* exited $got, want $want" "$out" "$err"
  fi
}

# has fails unless a file matches every extended regular expression given.
has() {
  local file=$1 re
  shift
  for re in "$@"; do
    grep -Eq -- "$re" "$file" || fail "$file does not match /$re/" "$file"
  done
}

# lacks fails if a file matches any extended regular expression given.
lacks() {
  local file=$1 re
  shift
  for re in "$@"; do
    if grep -Eq -- "$re" "$file"; then
      fail "$file matches /$re/" "$file"
    fi
  done
}

# eventually runs a command every two seconds, up to the number of tries
# given first, until it succeeds, and fails with the description given
# second if it never does.
eventually() {
  local tries=$1 what=$2 i
  shift 2
  for i in $(seq 1 "$tries"); do
    if "$@" > /dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  fail "timed out waiting for $what"
}

# serves reports whether a URL answers, writing what it sends to the file
# given, if any.
serves() {
  curl -fsS -o "${2:-/dev/null}" "$1" 2> /dev/null
}

# matches reports whether what a command prints matches an extended regular
# expression, given first: eventually matches '^Running$' kubectl get ...
matches() {
  local re=$1 got
  shift
  got=$("$@" 2> /dev/null) || return 1
  [[ $got =~ $re ]]
}
