# shellcheck shell=sh
# Runs on this ssh session's stdio: `sshfs -o slave` mounts the project dir R
# (served by rcc on A) at the same path, inside a private mount namespace.
# There the history profile PROF (if any) is also bound over the account's
# projects/ and history.jsonl (CFG), keeping machines' histories apart.
: "${R:?}" "${RUN:?}" "${PROF?}" "${CFG:?}"
set -e
# Files also change on A (Claude's commands and git run there), so sshfs'
# own directory cache (20s by default) and remembered "not found" answers
# would show stale state: off. The kernel keeps 1s of positive caching.
opts="slave,uid=$(id -u),gid=$(id -g),dir_cache=no,attr_timeout=1,entry_timeout=1,negative_timeout=0"
first="$R"
while [ ! -e "$(dirname "$first")" ]; do first=$(dirname "$first"); done
[ -e "$R" ] || printf '%s\n' "$first" > "$RUN/created"
echo $$ > "$RUN/pid"
# shellcheck disable=SC2016 # $1.. expand in the inner shell
exec unshare -m --propagation private sh -c '
  set -e
  mkdir -p "$1"
  if [ -n "$3" ]; then
    umask 077
    mkdir -p "$3/projects" "$4/projects"
    [ -e "$3/history.jsonl" ] || : > "$3/history.jsonl"
    [ -e "$4/history.jsonl" ] || : > "$4/history.jsonl"
    mount --bind "$3/projects" "$4/projects"
    mount --bind "$3/history.jsonl" "$4/history.jsonl"
  fi
  exec sshfs -f -o "$2" ":$1" "$1"' sh "$R" "$opts" "$PROF" "$CFG"
