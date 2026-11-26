#!/bin/sh
# remote-cc installer for Linux and macOS.
#
#   curl -fsSL https://raw.githubusercontent.com/NotDesigned/remote-cc/main/install.sh | sh
#
# Environment (all optional):
#   RCC_VERSION       release tag to install, e.g. v0.3.2 (default: latest)
#   RCC_INSTALL_DIR   target directory (default: ~/.local/bin)
#   RCC_NAME          command name (default: remote-cc)
#   RCC_REPO          GitHub owner/repo (default: NotDesigned/remote-cc)
set -eu

repo=${RCC_REPO:-NotDesigned/remote-cc}
dir=${RCC_INSTALL_DIR:-$HOME/.local/bin}
name=${RCC_NAME:-remote-cc}
version=${RCC_VERSION:-latest}

say() { printf '%s\n' "remote-cc: $*" >&2; }
die() { say "$*"; exit 1; }

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) die "unsupported OS $(uname -s); on Windows use install.ps1" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) die "unsupported architecture $(uname -m)" ;;
esac
asset="rcc-$os-$arch"

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL --retry 3 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q -O "$2" "$1"; }
else
  die "curl or wget is required"
fi
if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  die "sha256sum or shasum is required"
fi

if [ "$version" = latest ]; then
  base="https://github.com/$repo/releases/latest/download"
else
  case "$version" in v*) ;; *) version="v$version" ;; esac
  base="https://github.com/$repo/releases/download/$version"
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "downloading $asset ($version) from github.com/$repo"
fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS" || die "could not download SHA256SUMS (does release $version exist?)"
fetch "$base/$asset" "$tmp/$asset" || die "could not download $asset"

want=$(awk -v a="$asset" '$2 == a || $2 == "*" a { print $1 }' "$tmp/SHA256SUMS")
[ -n "$want" ] || die "no checksum for $asset in SHA256SUMS"
got=$(sha256 "$tmp/$asset")
[ "$got" = "$want" ] || die "checksum mismatch for $asset (got $got, want $want)"

mkdir -p "$dir"
chmod 755 "$tmp/$asset"
# Same-directory temp + rename: atomic, and safe while an old copy runs.
cp "$tmp/$asset" "$dir/.$name.new"
mv -f "$dir/.$name.new" "$dir/$name"
say "installed $("$dir/$name" version) to $dir/$name"

case ":$PATH:" in
  *":$dir:"*) ;;
  *)
    say "$dir is not in your PATH. Add it, e.g.:"
    case "${SHELL:-}" in
      */zsh) say "  echo 'export PATH=\"$dir:\$PATH\"' >> ~/.zshrc && exec zsh" ;;
      */fish) say "  fish_add_path $dir" ;;
      *) say "  echo 'export PATH=\"$dir:\$PATH\"' >> ~/.bashrc && exec bash" ;;
    esac
    ;;
esac

command -v ssh >/dev/null 2>&1 || say "warning: no ssh client found; remote-cc needs one"
command -v bash >/dev/null 2>&1 || say "warning: no bash found; commands from Claude run in bash on this machine"
say "next: $name server setup user@your-server   (see https://github.com/$repo#readme)"
