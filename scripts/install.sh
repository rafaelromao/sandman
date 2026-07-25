#!/bin/sh
set -eu

REPOSITORY="rafaelromao/sandman"
VERSION="${SANDMAN_VERSION:-v1.0.0-rc.1}"
INSTALL_DIR="${SANDMAN_INSTALL_DIR:-${HOME}/.local/bin}"

fail() {
    printf '%s\n' "install: $*" >&2
    exit 1
}

usage() {
    cat >&2 <<'EOF'
Usage: install.sh [--version VERSION] [--install-dir DIRECTORY]

Environment overrides:
  SANDMAN_VERSION       release tag, for example v1.0.0-rc.1
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

case "$(uname -s)" in
    Linux) OS=linux ;;
    Darwin) OS=darwin ;;
    *) fail "unsupported operating system: $(uname -s)" ;;
esac

case "$(uname -m)" in
    amd64|x86_64) ARCH=amd64 ;;
    arm64|aarch64) ARCH=arm64 ;;
    *) fail "unsupported architecture: $(uname -m)" ;;
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
