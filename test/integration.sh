#!/bin/sh
# End-to-end test of a remote-cc server, client and admin commands on one
# machine. CI ONLY: it installs packages, creates Linux users, Docker
# containers and networks, and edits ~/.ssh. Needs passwordless sudo, Docker,
#   RCC    the rcc binary to test
#   IMAGE  the server image (built locally)
# The machine is the Docker host (reached as $USER@localhost over ssh), the
# first user's machine (this account) and two more "machines" (Linux users
# alice and alice2).
set -eu
RCC=$(readlink -f "${RCC:?}")
TESTDIR=$(cd "$(dirname "$0")" && pwd)
IMAGE=${IMAGE:?}
ME=$(id -un)
ADMIN="$ME@localhost"

step() { printf '\n=== %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
as() { u=$1; shift; sudo -iu "$u" "$@"; }
expect_fail() { if "$@"; then fail "should have failed: $*"; fi; }
# rcc's session wants a terminal; script(1) provides one.
tty_run() { script -qefc "$*" /dev/null; }

step "prerequisites"
sudo apt-get install -y -qq openssh-server ripgrep >/dev/null
sudo systemctl start ssh 2>/dev/null || sudo service ssh start
chmod 755 "$HOME"
mkdir -p ~/.ssh && chmod 700 ~/.ssh
[ -f ~/.ssh/id_ed25519 ] || ssh-keygen -q -t ed25519 -N '' -f ~/.ssh/id_ed25519
cat ~/.ssh/id_ed25519.pub >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys
ssh-keyscan -t ed25519 localhost >> ~/.ssh/known_hosts 2>/dev/null
ssh -o BatchMode=yes "$ADMIN" true
sudo install -m 755 "$RCC" /usr/local/bin/remote-cc
for u in alice alice2 mallory; do id "$u" >/dev/null 2>&1 || sudo useradd -m -s /bin/bash "$u"; done
cat > /tmp/fake-claude <<'EOF'
#!/bin/sh
# Stands in for Claude Code: checks the mounted project and the command
# channel, then exits with the code given as "exit=N".
set -e
code=0
for a; do case $a in exit=*) code=${a#exit=} ;; esac; done
test "$(cat from-a.txt)" = hello-from-a
echo hello-from-b > from-b.txt
"$CLAUDE_CODE_SHELL_PREFIX" 'uname -n > ran-on.txt; pwd > pwd-on-a.txt'
test "$(cat ran-on.txt)" != "$(uname -n)"
exit "$code"
EOF
chmod 755 /tmp/fake-claude

step "server setup (through sudo)"
remote-cc server setup --image "$IMAGE" --user owner "$ADMIN"
alias=localhost-cc
[ "$(ssh -o BatchMode=yes "$alias" hostname)" = rcc-owner ] || fail "alias does not reach rcc-owner"
grep -q "Include remote-cc/config" ~/.ssh/config || fail "no Include in ~/.ssh/config"

step "session: files from here, commands run here, exit code"
sudo docker cp /tmp/fake-claude rcc-owner:/usr/local/bin/fake-claude
proj=$HOME/proj
mkdir -p "$proj" && echo hello-from-a > "$proj/from-a.txt"
tty_run "cd '$proj' && remote-cc --claude /usr/local/bin/fake-claude $alias"
[ "$(cat "$proj/from-b.txt")" = hello-from-b ] || fail "write through the mount"
[ "$(cat "$proj/ran-on.txt")" = "$(uname -n)" ] || fail "command did not run here"
[ "$(cat "$proj/pwd-on-a.txt")" = "$proj" ] || fail "command ran in the wrong directory"
set +e
tty_run "cd '$proj' && remote-cc --claude /usr/local/bin/fake-claude $alias -- exit=7"
rc=$?
set -e
[ "$rc" = 7 ] || fail "exit code $rc, want 7"
remote-cc account list "$alias"

step "session: a command survives a dropped connection"
cat > /tmp/fake-claude-slow <<'EOF'
#!/bin/sh
# Its command is still running on this machine when the connection drops.
set -e
"$CLAUDE_CODE_SHELL_PREFIX" 'sleep 5; echo survived > survived.txt'
test "$(cat survived.txt)" = survived
EOF
sudo docker cp /tmp/fake-claude-slow rcc-owner:/usr/local/bin/fake-claude-slow
sudo docker exec rcc-owner chmod 755 /usr/local/bin/fake-claude-slow
rm -f "$proj/survived.txt"
# The shared ssh connection's process is titled "ssh: <ControlPath> [mux]".
( sleep 2; pkill -KILL -f '^ssh: .*/rcc/cm/[0-9a-f]+ \[mux\]' ) &
tty_run "cd '$proj' && remote-cc --claude /usr/local/bin/fake-claude-slow $alias" | tee /tmp/drop.out
wait
grep -q "connection lost" /tmp/drop.out || fail "the dropped connection went unnoticed"
[ "$(cat "$proj/survived.txt")" = survived ] || fail "the command was lost in the reconnect"
[ "$(sudo docker exec rcc-owner sh -c 'ls /root/.cache/rcc/run | wc -l')" = 0 ] || fail "the session left its run directory"

step "real Claude Code (mock API): Bash tool, cwd, --worktree through local git, Grep through local rg"
gwip=$(sudo docker network inspect remote-cc -f '{{(index .IPAM.Config 0).Gateway}}')
rm -f /tmp/mock-results
python3 "$TESTDIR/mockapi.py" --listen "$gwip:8765" --log /tmp/mock-results \
  --command 'uname -n; pwd' --command 'mkdir -p sub && cd sub && git rev-parse --abbrev-ref HEAD' --command pwd \
  --command 'echo rcc-needle > found.txt' --tool 'Grep {"pattern": "rcc-needl[e]"}' \
  > /tmp/mockapi.log 2>&1 &
mock=$!
printf '#!/bin/sh\nexport ANTHROPIC_BASE_URL=http://%s:8765 ANTHROPIC_API_KEY=sk-ant-test CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1\nexec claude "$@"\n' "$gwip" > /tmp/claude-mock
chmod 755 /tmp/claude-mock
sudo docker cp /tmp/claude-mock rcc-owner:/usr/local/bin/claude-mock
repo=$HOME/repo
mkdir -p "$repo" && git -C "$repo" init -q && git -C "$repo" -c user.email=t@example.com -c user.name=t commit -q --allow-empty -m init
tty_run "cd '$repo' && remote-cc --claude /usr/local/bin/claude-mock $alias -- -w wt1 -p go --dangerously-skip-permissions --allowedTools Grep"
kill "$mock" 2>/dev/null || true
cat /tmp/mock-results
wt="$repo/.claude/worktrees/wt1"
r1=$(sed -n 1p /tmp/mock-results)
r2=$(sed -n 2p /tmp/mock-results)
r3=$(sed -n 3p /tmp/mock-results)
case "$r1" in *"$(uname -n)"*"$wt"*) ;; *) fail "Bash tool did not run here, in the worktree: $r1" ;; esac
case "$r2" in *worktree-wt1*) ;; *) fail "git in the worktree: $r2" ;; esac
case "$r3" in *"$wt/sub"*) ;; *) fail "cwd not kept between commands: $r3" ;; esac
git -C "$repo" worktree list | grep -q "$wt" || fail "worktree not created on this machine"
grep -q "cmd=git " ~/.cache/rcc/last.log || fail "Claude Code's git calls did not run here"
r5=$(sed -n 5p /tmp/mock-results)
case "$r5" in *'"is_error": false'*found.txt*) ;; *) fail "Grep tool: $r5" ;; esac
grep -q "cmd=rg .*rcc-needl" ~/.cache/rcc/last.log || fail "Claude Code's Grep did not run here"

step "server user add: invite, device join on another machine"
inv=$(remote-cc server user add "$ADMIN" alice | grep '^rcc1\.')
as alice remote-cc device join "$inv"
[ "$(as alice ssh -o BatchMode=yes "$alias" hostname)" = rcc-alice ] || fail "alice"
expect_fail as mallory remote-cc device join "$inv"
remote-cc server user list "$ADMIN" | grep -q alice || fail "server user list"

step "isolation"
expect_fail as alice ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  -J "$alias-gw" root@rcc-owner true
expect_fail as alice ssh -o BatchMode=yes "$alias-gw" true
expect_fail as alice ssh -o BatchMode=yes "$alias-gw" "invite ssh-ed25519 x 0123456789abcdef 1; id"
as alice ssh -o BatchMode=yes "$alias-gw" hostkey | grep -q '^ssh-ed25519 ' || fail "hostkey"

step "self-service devices"
dinv=$(as alice remote-cc device invite --ttl 2h "$alias" | grep '^rcc1\.')
as alice remote-cc device list "$alias" | tee /tmp/devs | grep -qi invite || fail "pending invite not listed"
as alice2 remote-cc device join "$dinv"
[ "$(as alice2 ssh -o BatchMode=yes "$alias" hostname)" = rcc-alice ] || fail "alice2"
expect_fail as mallory remote-cc device join "$dinv"
fp=$(as alice2 ssh-keygen -lf ".ssh/remote-cc/${alias}_ed25519" | awk '{print $2}')
as alice remote-cc device list "$alias" | grep -qF "$fp" || fail "device not listed"
as alice remote-cc device remove "$alias" "$fp"
expect_fail as alice2 ssh -o BatchMode=yes "$alias" true
[ "$(as alice ssh -o BatchMode=yes "$alias" hostname)" = rcc-alice ] || fail "alice lost access"
# Invites are capped per user.
for _ in 1 2 3 4 5; do as alice remote-cc device invite "$alias" >/dev/null 2>&1 || true; done
expect_fail as alice remote-cc device invite "$alias"

step "server user config / remove"
remote-cc server user config "$ADMIN" alice >/dev/null
remote-cc server user remove --purge "$ADMIN" alice
expect_fail as alice ssh -o BatchMode=yes "$alias" true
expect_fail sudo docker inspect rcc-alice >/dev/null 2>&1

step "server user rebuild: a fresh container, same user"
before=$(sudo docker inspect -f '{{.Id}}' rcc-owner)
remote-cc server user rebuild "$ADMIN" owner
[ "$(sudo docker inspect -f '{{.Id}}' rcc-owner)" != "$before" ] || fail "rcc-owner was not recreated"
[ "$(ssh -o BatchMode=yes "$alias" hostname)" = rcc-owner ] || fail "the rebuilt container is unreachable"
expect_fail remote-cc server user rebuild "$ADMIN" nobody

step "a stopped container: clear error, server update starts it"
sudo docker stop rcc-owner >/dev/null
if out=$(cd "$proj" && remote-cc --claude /usr/local/bin/fake-claude "$alias" 2>&1); then fail "connected to a stopped container"; fi
case "$out" in *"could not reach your container"*) ;; *) fail "no hint for a stopped container: $out" ;; esac
remote-cc server update --image "$IMAGE" "$ADMIN" | tee /tmp/upd
grep -q "started stopped container(s): rcc-owner" /tmp/upd || fail "server update did not start rcc-owner"
[ "$(ssh -o BatchMode=yes "$alias" hostname)" = rcc-owner ] || fail "after start"

step "server update"
remote-cc server update --image "$IMAGE" "$ADMIN"
[ "$(ssh -o BatchMode=yes "$alias" hostname)" = rcc-owner ] || fail "after update"

step "server migrate: a 0.4 single-container deployment"
ssh-keygen -q -t ed25519 -N '' -f ~/.ssh/single_ed25519
sdir=/opt/rcc-single sdata=/var/lib/rcc-single
sudo mkdir -p "$sdir"
sudo cp "$TESTDIR/testdata/compose.single.yml" "$sdir/compose.yml"
printf 'RCC_IMAGE=%s\nRCC_NAME=rccsingle\nRCC_DATA=%s\nRCC_PORT=2223\nRCC_PUBLISH=2223:22\nAUTHORIZED_KEYS=%s\n' \
  "$IMAGE" "$sdata" "$(cat ~/.ssh/single_ed25519.pub)" | sudo tee "$sdir/.env" >/dev/null
(cd "$sdir" && sudo docker compose up -d)
cat > ~/.ssh/single.conf <<EOF
Host single-old
    HostName localhost
    Port 2223
    User root
    HostKeyAlias single
Host single-gw
    HostName localhost
    Port 2223
    User bob
    HostKeyAlias single
Host single
    HostName rcc-bob
    User root
    ProxyJump single-gw
Host *
    IdentityFile ~/.ssh/single_ed25519
    IdentitiesOnly yes
    BatchMode yes
    UserKnownHostsFile ~/.ssh/single_known
    StrictHostKeyChecking accept-new
EOF
for _ in $(seq 30); do ssh -F ~/.ssh/single.conf single-old true 2>/dev/null && break; sleep 1; done
ssh -F ~/.ssh/single.conf single-old 'echo kept > /root/marker'
expect_fail remote-cc server init --install-dir "$sdir" "$ADMIN"
remote-cc server migrate --install-dir "$sdir" --name rccs --user bob --image "$IMAGE" "$ADMIN"
# Same host key (StrictHostKeyChecking would refuse a changed one), same data.
[ "$(ssh -F ~/.ssh/single.conf single cat /root/marker)" = kept ] || fail "migrated data"

step "server update --all (the main and the migrated server)"
remote-cc server list | tee /tmp/servers
[ "$(grep -c "$ADMIN" /tmp/servers)" = 2 ] || fail "server list should list both installs"
remote-cc server update --all --image "$IMAGE"
[ "$(ssh -o BatchMode=yes "$alias" hostname)" = rcc-owner ] || fail "after update --all"
[ "$(ssh -F ~/.ssh/single.conf single cat /root/marker)" = kept ] || fail "migrated server after update --all"
remote-cc server remove --purge --install-dir "$sdir" "$ADMIN"

step "server remove (it removes the image too, so it comes last)"
remote-cc server remove --purge "$ADMIN"
expect_fail sudo docker inspect remote-cc-gateway >/dev/null 2>&1
expect_fail sudo docker inspect rcc-owner >/dev/null 2>&1
remote-cc server list | grep -q "no servers" || fail "server remove should forget the server"

step "all passed"
