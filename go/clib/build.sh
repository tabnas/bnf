#!/bin/sh
# Build libtabnasbnf, the C-ABI shared library, for one or more targets.
#
#   ./build.sh                 # host only, into ./dist
#   ./build.sh all             # every target this host can reach
#   ./build.sh linux/arm64 …   # named targets
#
# CROSS-COMPILING USES ZIG. The library needs cgo, and cgo needs a C
# toolchain per target — which is what normally forces a CI matrix of
# native runners. `zig cc` is a cross compiler for all of them, so one
# Linux box can produce Linux and Windows artifacts. Point ZIG at the
# binary, or have it on PATH:
#
#   curl -L https://ziglang.org/download/<ver>/zig-<host>-<ver>.tar.xz | tar xJ
#   ZIG=./zig-<host>-<ver>/zig ./build.sh all
#
# macOS is the exception and cannot be cross-compiled this way: linking
# needs Apple's SDK (CoreFoundation, libresolv), which zig cannot
# redistribute. Build darwin artifacts on a macOS host, where the plain
# system toolchain works and ZIG is not needed.
set -eu

ZIG="${ZIG:-zig}"
OUT="${OUT:-dist}"
PKG="."

host_os=$(go env GOOS)
host_arch=$(go env GOARCH)

# A target NAMED on the command line is a requirement, not a wish: if it
# cannot be built the script fails, so release automation cannot mistake
# an incomplete artifact set for a successful build. `all` stays
# best-effort, because its whole point is "whatever this host can reach".
targets=""
explicit=0
case "${1:-host}" in
  host) targets="$host_os/$host_arch" ;;
  all)  targets="linux/amd64 linux/arm64 windows/amd64"
        # Only offer darwin when we are ON darwin; see the note above.
        [ "$host_os" = "darwin" ] && targets="$targets darwin/amd64 darwin/arm64" ;;
  *)    targets="$*"; explicit=1 ;;
esac

# skip_or_fail <message>: a skip when the target set was inferred, a hard
# failure when the caller asked for this target by name.
skip_or_fail() {
  echo "$1" >&2
  [ "$explicit" = "1" ] && exit 1
  return 0
}

# zig's target triple and the shared-library extension for a Go target.
zig_target() {
  case "$1/$2" in
    linux/amd64)   echo "x86_64-linux-gnu" ;;
    linux/arm64)   echo "aarch64-linux-gnu" ;;
    windows/amd64) echo "x86_64-windows-gnu" ;;
    windows/arm64) echo "aarch64-windows-gnu" ;;
    *)             echo "" ;;
  esac
}

lib_ext() {
  case "$1" in
    windows) echo ".dll" ;;
    darwin)  echo ".dylib" ;;
    *)       echo ".so" ;;
  esac
}

mkdir -p "$OUT"

for t in $targets; do
  os=${t%%/*}
  arch=${t##*/}
  ext=$(lib_ext "$os")
  out="$OUT/libtabnasbnf-$os-$arch$ext"

  if [ "$os" = "$host_os" ] && [ "$arch" = "$host_arch" ]; then
    # Native: the system toolchain is already correct, and on macOS it is
    # the only one that can link.
    CGO_ENABLED=1 GOOS="$os" GOARCH="$arch" \
      go build -buildmode=c-shared -o "$out" "$PKG"
  else
    if [ "$os" = "darwin" ]; then
      skip_or_fail "skip $t: darwin cannot be cross-compiled (needs Apple's SDK); build on a macOS host"
      continue
    fi
    zt=$(zig_target "$os" "$arch")
    if [ -z "$zt" ]; then
      skip_or_fail "skip $t: no zig target mapping"
      continue
    fi
    if ! command -v "$ZIG" >/dev/null 2>&1 && [ ! -x "$ZIG" ]; then
      skip_or_fail "skip $t: zig not found (set ZIG=/path/to/zig)"
      continue
    fi
    # go passes the compiler as one word, so the -target flag needs a
    # wrapper rather than being appended to CC.
    cc="$OUT/.zigcc-$os-$arch"
    printf '#!/bin/sh\nexec %s cc -target %s "$@"\n' "$ZIG" "$zt" > "$cc"
    chmod +x "$cc"
    CGO_ENABLED=1 GOOS="$os" GOARCH="$arch" CC="$(cd "$(dirname "$cc")" && pwd)/$(basename "$cc")" \
      go build -buildmode=c-shared -o "$out" "$PKG"
    rm -f "$cc"
  fi

  echo "built $out"
done
