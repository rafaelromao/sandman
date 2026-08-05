#!/bin/sh
set -eu

REPOSITORY="rafaelromao/sandman"
VERSION="${SANDMAN_VERSION:-}"
INSTALL_DIR="${SANDMAN_INSTALL_DIR:-${HOME}/.local/bin}"
INCLUDE_PRERELEASE=0

fail() {
    printf '%s\n' "install: $*" >&2
    exit 1
}

usage() {
    cat >&2 <<'EOF'
Usage: install.sh [--version VERSION] [--install-dir DIRECTORY] [--include-prerelease]

Environment overrides:
  SANDMAN_VERSION       release tag, for example v1.2.3
  SANDMAN_INSTALL_DIR   destination directory (default: ~/.local/bin)
EOF
    exit 2
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --version)
            [ "$#" -ge 2 ] || usage
            VERSION=$2
            shift 2
            ;;
        --install-dir)
            [ "$#" -ge 2 ] || usage
            INSTALL_DIR=$2
            shift 2
            ;;
        --include-prerelease)
            INCLUDE_PRERELEASE=1
            shift
            ;;                        
        -h|--help)
            usage
            ;;
        *)
            usage
            ;;
    esac
done

command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v tar >/dev/null 2>&1 || fail "tar is required"

if [ -z "$VERSION" ]; then
    VERSION=$(curl -fsSL "https://api.github.com/repos/${REPOSITORY}/releases" |
        awk -v include_prerelease="$INCLUDE_PRERELEASE" '
            /"tag_name":/ {
                tag = $0; gsub(/.*"tag_name": *"/, "", tag); gsub(/".*/, "", tag)
                next
            }
            /"prerelease":/ {
                pre = ($0 ~ /true/) ? 1 : 0
                if (include_prerelease || !pre) {
                    print tag
                    exit
                }
                next
            }
        ') ||
        fail "could not determine the latest release"
    [ -n "$VERSION" ] || fail "no releases found; use --version to specify a release tag, or --include-prerelease to include prereleases"
fi

case "$(uname -s)" in
    Linux) OS=linux ;;
    Darwin) OS=darwin ;;
    *) fail "unsupported operating system: $(uname -s); supported platforms: linux/amd64, linux/arm64, darwin/amd64, darwin/arm64" ;;
esac

case "$(uname -m)" in
    amd64|x86_64) ARCH=amd64 ;;
    arm64|aarch64) ARCH=arm64 ;;
    *) fail "unsupported architecture: $(uname -m); supported platforms: linux/amd64, linux/arm64, darwin/amd64, darwin/arm64" ;;
esac

case "$VERSION" in
    v*) VERSION_NUMBER=${VERSION#v} ;;
    *) VERSION_NUMBER=$VERSION ;;
esac

ARCHIVE="sandman_${VERSION_NUMBER}_${OS}_${ARCH}.tar.gz"
BASE_URL="https://github.com/${REPOSITORY}/releases/download/${VERSION}"
TEMP_DIR=$(mktemp -d 2>/dev/null || mktemp -d -t sandman-install)
trap 'rm -rf "$TEMP_DIR"' EXIT INT TERM

curl -fsSL -o "${TEMP_DIR}/${ARCHIVE}" "${BASE_URL}/${ARCHIVE}" || \
    fail "could not download ${ARCHIVE}"
curl -fsSL -o "${TEMP_DIR}/checksums.txt" "${BASE_URL}/checksums.txt" || \
    fail "could not download checksums.txt"

grep -F "  ${ARCHIVE}" "${TEMP_DIR}/checksums.txt" > "${TEMP_DIR}/checksum-entry" || \
    fail "checksum entry not found for ${ARCHIVE}"

(
    cd "$TEMP_DIR"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum -c checksum-entry >/dev/null
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 -c checksum-entry >/dev/null
    else
        fail "sha256sum or shasum is required for checksum verification"
    fi
) || fail "checksum verification failed"

tar -xzf "${TEMP_DIR}/${ARCHIVE}" -C "$TEMP_DIR" || \
    fail "could not extract ${ARCHIVE}"
mkdir -p "$INSTALL_DIR"
install -m 755 "${TEMP_DIR}/sandman" "${INSTALL_DIR}/sandman" || \
    fail "could not install sandman into ${INSTALL_DIR}"

printf 'Installed sandman %s in %s\n' "$VERSION" "$INSTALL_DIR"
