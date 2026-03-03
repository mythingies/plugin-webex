#!/bin/sh
set -euo pipefail
REPO="mythingies/plugin-webex"

VERSION="${1:-}"
INSTALL_DIR="${HOME}/.local/bin"
BINARY="webex-mcp"

# --- helpers ---------------------------------------------------------------

log()   { printf '%s\n' "$@"; }
die()   { log "ERROR: $*" >&2; exit 1; }

need() {
  command -v "$1" >/dev/null 2>&1 || die "'$1' is required but not found"
}

# --- detect platform -------------------------------------------------------

detect_os() {
  case "$(uname -s)" in
    Linux*)  echo "linux"  ;;
    Darwin*) echo "darwin" ;;
    *)       die "Unsupported OS: $(uname -s)" ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)   echo "amd64" ;;
    aarch64|arm64)   echo "arm64" ;;
    *)               die "Unsupported architecture: $(uname -m)" ;;
  esac
}

# --- resolve version -------------------------------------------------------

resolve_version() {
  if [ -n "$VERSION" ]; then
    echo "$VERSION"
    return
  fi

  need curl
  local latest
  latest=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep '"tag_name"' \
    | head -1 \
    | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/')

  [ -n "$latest" ] || die "Could not determine latest release"
  echo "$latest"
}

# --- download & verify -----------------------------------------------------

download_and_install() {
  need curl
  if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
    die "'sha256sum' or 'shasum' is required but neither was found"
  fi

  local os arch version archive url checksums_url tmpdir
  os="$(detect_os)"
  arch="$(detect_arch)"
  version="$(resolve_version)"

  archive="${BINARY}-${os}-${arch}.tar.gz"
  url="https://github.com/${REPO}/releases/download/${version}/${archive}"
  checksums_url="https://github.com/${REPO}/releases/download/${version}/checksums.txt"

  tmpdir="$(mktemp -d)"
  trap 'rm -rf "$tmpdir"' EXIT

  log "Downloading ${BINARY} ${version} (${os}/${arch})..."
  curl -fsSL -o "${tmpdir}/${archive}" "$url" \
    || die "Download failed — does release ${version} exist?"
  curl -fsSL -o "${tmpdir}/checksums.txt" "$checksums_url" \
    || die "Checksum file download failed"

  # verify checksum
  log "Verifying checksum..."
  local expected actual
  expected=$(grep "${archive}" "${tmpdir}/checksums.txt" | awk '{print $1}')
  [ -n "$expected" ] || die "No checksum found for ${archive}"

  if command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum "${tmpdir}/${archive}" | awk '{print $1}')
  else
    actual=$(shasum -a 256 "${tmpdir}/${archive}" | awk '{print $1}')
  fi

  [ "$expected" = "$actual" ] || die "Checksum mismatch: expected ${expected}, got ${actual}"
  log "Checksum OK."

  # extract
  mkdir -p "$INSTALL_DIR"
  tar -xzf "${tmpdir}/${archive}" -C "${tmpdir}"
  mv "${tmpdir}/${BINARY}" "${INSTALL_DIR}/${BINARY}"
  chmod +x "${INSTALL_DIR}/${BINARY}"

  log ""
  log "Installed ${BINARY} to ${INSTALL_DIR}/${BINARY}"

  # PATH hint
  case ":${PATH}:" in
    *":${INSTALL_DIR}:"*) ;;
    *)
      log ""
      log "NOTE: ${INSTALL_DIR} is not in your PATH."
      log "Add it by appending this to your shell profile (~/.bashrc, ~/.zshrc, etc.):"
      log ""
      log "  export PATH=\"${INSTALL_DIR}:\$PATH\""
      ;;
  esac
}

# --- main ------------------------------------------------------------------

download_and_install
