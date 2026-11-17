#!/usr/bin/env bash
# Rebuilds ../treesitter.wasm from pinned upstream sources. Run it only when a
# version below changes; the committed .wasm is what `go build` embeds, so
# building brisk itself needs no C toolchain.
#
# Needs network access, curl, patch, node with npm (for tree-sitter-cli, whose
# install script downloads its binary, so npm must not set ignore-scripts),
# and zig (https://ziglang.org) on PATH, or `pip install ziglang==$ZIG_VERSION`
# and ZIG="python3 -m ziglang".
set -euo pipefail

TREE_SITTER=v0.27.2
TREE_SITTER_GO=v0.25.0
TREE_SITTER_TYPESCRIPT=v0.23.2
ZIG_VERSION=0.17.1
TREE_SITTER_CLI=0.25.8 # regenerates tree-sitter-go after the patch; needs node
ZIG=${ZIG:-zig}

here=$(cd "$(dirname "$0")" && pwd)
out=$(dirname "$here")
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

fetch() { # repo tag
  mkdir -p "$work/$1"
  curl -sSfL "https://github.com/tree-sitter/$1/archive/refs/tags/$2.tar.gz" |
    tar -xz -C "$work/$1" --strip-components=1
}
fetch tree-sitter "$TREE_SITTER"
fetch tree-sitter-go "$TREE_SITTER_GO"
fetch tree-sitter-typescript "$TREE_SITTER_TYPESCRIPT"

# Go 1.26 allows new(expr); upstream tree-sitter-go only parses new(Type).
# tree-sitter-cli $TREE_SITTER_CLI regenerates upstream's parser.c byte for
# byte without the patch, so the patched parser is reproducible.
patch --batch --fuzz=0 -d "$work/tree-sitter-go" -p1 <"$here/tree-sitter-go-new-expr.patch"
npm install --silent --no-save --prefix "$work/cli" "tree-sitter-cli@$TREE_SITTER_CLI"
(cd "$work/tree-sitter-go" && "$work/cli/node_modules/.bin/tree-sitter" generate)

have=$($ZIG version)
if [ "$have" != "$ZIG_VERSION" ]; then
  echo "warning: zig $have, pinned $ZIG_VERSION; output may differ" >&2
fi

ts=$work/tree-sitter/lib
tsg=$work/tree-sitter-typescript
$ZIG cc --target=wasm32-wasi-musl -mexec-model=reactor -O2 -fno-sanitize=undefined \
  -I"$ts/include" -I"$ts/src" \
  "$ts/src/lib.c" \
  -I"$work/tree-sitter-go/src" "$work/tree-sitter-go/src/parser.c" \
  -I"$tsg/typescript/src" "$tsg/typescript/src/parser.c" "$tsg/typescript/src/scanner.c" \
  "$tsg/tsx/src/parser.c" "$tsg/tsx/src/scanner.c" \
  "$here/shim.c" \
  -Wl,--no-entry -Wl,--strip-all -Wl,-z,stack-size=1048576 \
  -o "$out/treesitter.wasm"

cat >"$out/VERSIONS" <<EOF
tree-sitter $TREE_SITTER
tree-sitter-go $TREE_SITTER_GO + tree-sitter-go-new-expr.patch (tree-sitter-cli $TREE_SITTER_CLI)
tree-sitter-typescript $TREE_SITTER_TYPESCRIPT
zig $have
EOF
ls -l "$out/treesitter.wasm"
