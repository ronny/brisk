#!/usr/bin/env bash
# Installs the brisk binary that .amp/plugins/brisk runs, at the release in
# .amp/plugins/brisk/BRISK_VERSION, and points the plugin at it in the user's
# Amp settings. .agents/setup runs it in orbs; run it once by hand on a runner
# machine, and again after bumping BRISK_VERSION. Idempotent.
#
# brisk rates the changes Amp threads make to brisk itself with a released
# binary and a released copy of the plugin, never the code being changed, so a
# change to rules/, risk/ or adapters/amp/ cannot change how it is rated. The
# binary is checked against .amp/plugins/brisk/SHA256SUMS, committed with the
# pin, so a release asset replaced later is refused.
#
# The plugin reads its settings from ~/.config/amp/settings.json, then
# settings.jsonc, then the machine's managed settings (never the repo's
# .amp/settings.json). The script writes settings.json, so it refuses to run
# when a settings.jsonc would override it. These keys apply to every project
# on the machine:
#   preset light       a person is asked only on strong single signals
#   backend amp        Amp's Jev answers brisk's questions, not OpenRouter
#   askPerson note     advisory: label and Ship line, no dialog before Ship
#   confirmUnassessed  off: Ship never waits on a person
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
pin="$repo_root/.amp/plugins/brisk"
tag="$(tr -d '[:space:]' < "$pin/BRISK_VERSION")"
[[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "bad brisk version: $tag" >&2; exit 1; }

case "$(uname -s)" in Linux) os=linux ;; Darwin) os=darwin ;; *) echo "unsupported OS" >&2; exit 1 ;; esac
case "$(uname -m)" in x86_64 | amd64) arch=amd64 ;; arm64 | aarch64) arch=arm64 ;; *) echo "unsupported arch" >&2; exit 1 ;; esac
asset="brisk-${tag}-${os}-${arch}"

settings="$HOME/.config/amp/settings.json"
if [ -e "${settings}c" ]; then
  echo "${settings}c overrides $settings; set the amp.brisk.* keys from this script there instead" >&2
  exit 1
fi
command -v jq > /dev/null || { echo "jq is required" >&2; exit 1; }

dir="$HOME/.local/share/brisk/$tag"
if ! "$dir/brisk" --version 2> /dev/null | grep -qx "brisk $tag"; then
  sum="$(grep " ${asset}\$" "$pin/SHA256SUMS")" || { echo "$asset is not in $pin/SHA256SUMS" >&2; exit 1; }
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  # gh carries the credentials in orbs and on runners while ronny/brisk is private.
  gh release download "$tag" -R ronny/brisk -D "$tmp" -p "$asset"
  if command -v sha256sum > /dev/null; then
    (cd "$tmp" && echo "$sum" | sha256sum -c -)
  else
    (cd "$tmp" && echo "$sum" | shasum -a 256 -c -)
  fi
  mkdir -p "$dir"
  install -m 0755 "$tmp/$asset" "$dir/brisk"
fi
"$dir/brisk" --version | grep -qx "brisk $tag" || { echo "$dir/brisk does not report $tag" >&2; exit 1; }
# For running brisk by hand; the plugin uses the versioned path.
mkdir -p "$HOME/.local/bin"
ln -sfn "$dir/brisk" "$HOME/.local/bin/brisk"

mkdir -p "$(dirname "$settings")"
[ -s "$settings" ] || echo '{}' > "$settings"
merged="$(jq --arg bin "$dir/brisk" '. + {
  "amp.brisk.binary": $bin,
  "amp.brisk.backend": "amp",
  "amp.brisk.preset": "light",
  "amp.brisk.askPerson": "note",
  "amp.brisk.confirmUnassessed": false
}' "$settings")"
# Rewrite in place, so a symlinked settings.json stays a symlink.
printf '%s\n' "$merged" > "$settings"

echo "brisk $tag at $dir/brisk"
