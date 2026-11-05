#!/bin/sh
# Creates the named directory under RUNNERS_BASE_PATH, then downloads, verifies and extracts the GitHub Actions runner.
#
# Usage: install-runner.sh <runner_name> [version]
# Environment variables:
#   RUNNERS_BASE_PATH      runner root directory, default /app/runners
#   RUNNER_VERSION         version number (without v), same as the second argument
#   RUNNER_SHA256          expected tarball SHA256; use in offline or self-hosted mirror setups
#   RUNNER_FORCE_REINSTALL set to 1 to re-download even if a runner already exists in the directory
#
# Differences from the old version:
#   - Selects the architecture via uname -m instead of hardcoding x64 (installing the x64 package on arm64 machines would fail to run)
#   - If no version is given, queries GitHub for the latest release, falling back to the built-in version if that fails
#   - SHA256 is always verified for every version; if the official hash can't be obtained, the script fails instead of silently skipping verification

set -e

RUNNER_NAME="${1:?Usage: install-runner.sh <runner_name> [version]}"
BASE="${RUNNERS_BASE_PATH:-/app/runners}"

detect_os() {
    case "$(uname -s)" in
        Linux) echo "linux" ;;
        Darwin) echo "osx" ;;
        *) echo "" ;;
    esac
}

# macOS ships shasum instead of sha256sum
verify_sha256() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum -c -
    else
        shasum -a 256 -c -
    fi
}

# Fallback version used when the latest release can't be queried, and its official SHA256 (x64 only)
FALLBACK_VERSION="2.331.0"
FALLBACK_SHA256_X64="5fcc01bd546ba5c3f1291c2803658ebd3cedb3836489eda3be357d41bfcf28a7"

API="https://api.github.com/repos/actions/runner/releases"

detect_arch() {
    case "$(uname -m)" in
        x86_64 | amd64) echo "x64" ;;
        aarch64 | arm64) echo "arm64" ;;
        armv7l | armv7) echo "arm" ;;
        *) echo "" ;;
    esac
}

# resolve_latest gets the latest release version (with the v prefix removed); outputs empty on failure
resolve_latest() {
    curl -fsSL --max-time 20 "${API}/latest" 2>/dev/null |
        sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"v\([^"]*\)".*/\1/p' |
        head -n 1
}

# release_json fetches the release JSON for the given version (requested only once, shared by the two parsing methods below)
release_json() {
    curl -fsSL --max-time 20 "${API}/tags/v$1" 2>/dev/null
}

# sha256_from_assets gets the digest from the release asset metadata (in the form "digest": "sha256:<hash>").
# GitHub provides this field on assets, making it a more reliable source than parsing the release notes body.
sha256_from_assets() {
    awk -v want="$1" '
        # Re-check whether the current asset is the target one each time a "name" field is encountered
        /"name"[[:space:]]*:/ { inasset = (index($0, "\"" want "\"") > 0) }
        inasset && /"digest"[[:space:]]*:[[:space:]]*"sha256:/ {
            # Avoids the {64} interval quantifier so the script does not depend on awk supporting interval expressions
            if (match($0, /sha256:[0-9a-f]+/)) {
                h = substr($0, RSTART + 7, RLENGTH - 7)
                if (length(h) == 64) { print h; exit }
            }
        }
    '
}

# sha256_from_body gets "<hash>  <tarball>" from the release notes body (the format used in the official install snippets)
sha256_from_body() {
    _tarball_re=$(printf '%s' "$1" | sed 's/\./\\./g')
    grep -oE "[0-9a-f]{64}[[:space:]]+${_tarball_re}" | head -n 1 | cut -c1-64
}

# resolve_sha256 gets the official SHA256 for the given tarball: check the asset digest first, then the release notes body;
# If neither source provides it, output nothing and let the caller decide how to handle it
resolve_sha256() {
    _json=$(release_json "$1")
    [ -n "$_json" ] || return 0
    _sum=$(printf '%s' "$_json" | sha256_from_assets "$2")
    [ -n "$_sum" ] || _sum=$(printf '%s' "$_json" | sha256_from_body "$2")
    printf '%s' "$_sum"
}

ARCH="$(detect_arch)"
if [ -z "$ARCH" ]; then
    echo "Unsupported architecture: $(uname -m)" >&2
    exit 1
fi

VERSION="${2:-${RUNNER_VERSION:-}}"
if [ -z "$VERSION" ]; then
    echo "Checking for the latest actions/runner version..."
    VERSION="$(resolve_latest || true)"
    if [ -z "$VERSION" ]; then
        VERSION="$FALLBACK_VERSION"
        echo "Lookup failed, falling back to version ${VERSION}"
    fi
fi

OS="$(detect_os)"
if [ -z "$OS" ]; then
    echo "Unsupported system: $(uname -s)" >&2
    exit 1
fi

TARBALL="actions-runner-${OS}-${ARCH}-${VERSION}.tar.gz"
URL="https://github.com/actions/runner/releases/download/v${VERSION}/${TARBALL}"
INSTALL_DIR="${BASE}/${RUNNER_NAME}"

if [ -f "${INSTALL_DIR}/config.sh" ] && [ "${RUNNER_FORCE_REINSTALL:-}" != "1" ]; then
    echo "A runner already exists under ${INSTALL_DIR}, skipping download (set RUNNER_FORCE_REINSTALL=1 to reinstall)"
    exit 0
fi

# Verify hash: explicit value > official value in release notes > built-in value for the fallback version
EXPECTED="${RUNNER_SHA256:-}"
if [ -z "$EXPECTED" ]; then
    EXPECTED="$(resolve_sha256 "$VERSION" "$TARBALL" || true)"
fi
if [ -z "$EXPECTED" ] && [ "$OS" = "linux" ] && [ "$VERSION" = "$FALLBACK_VERSION" ] && [ "$ARCH" = "x64" ]; then
    EXPECTED="$FALLBACK_SHA256_X64"
fi
if [ -z "$EXPECTED" ]; then
    echo "Unable to get the official SHA256 for ${TARBALL} (network unreachable, or this version/architecture does not exist)." >&2
    echo "Get the hash from https://github.com/actions/runner/releases/tag/v${VERSION} and retry:" >&2
    echo "  RUNNER_SHA256=<sha256> $0 ${RUNNER_NAME} ${VERSION}" >&2
    exit 1
fi

mkdir -p "$INSTALL_DIR"
cd "$INSTALL_DIR"
# When BASE is a relative path (the repo-local default is ./runners), after cd the trap's
# "${INSTALL_DIR}/${TARBALL}" is resolved again relative to the new cwd, so it fails to delete the real tar package.
# Convert to an absolute path here; the trap and later messages both use it.
INSTALL_DIR="$(pwd)"

# Don't leave a half-downloaded package behind if the download fails or exits midway, to avoid misjudgment next time
trap 'rm -f "${INSTALL_DIR}/${TARBALL}"' EXIT

echo "Downloading ${TARBALL} ..."
# -f: fail directly on HTTP errors instead of writing an error page as the tar package
curl -fSL --retry 3 -o "$TARBALL" "$URL"

echo "Verifying SHA256 ..."
echo "${EXPECTED}  ${TARBALL}" | verify_sha256

echo "Extracting..."
tar xzf "$TARBALL"

echo "Done. Install directory: ${INSTALL_DIR} (version ${VERSION}, architecture ${ARCH})"
echo "In the admin UI, use 'Quick Add Runner' and fill in name: ${RUNNER_NAME}, target and token to complete registration."
