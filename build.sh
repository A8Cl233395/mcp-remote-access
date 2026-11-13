#!/usr/bin/env bash
set -euo pipefail

CLI_DEFAULT_SERVER=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        -DefaultServer|--default-server|-s|--server)
            if [[ $# -lt 2 ]]; then
                echo "Error: $1 requires a value" >&2
                exit 1
            fi
            CLI_DEFAULT_SERVER="$2"
            shift 2
            ;;
        --default-server=*)
            CLI_DEFAULT_SERVER="${1#*=}"
            shift
            ;;
        -h|--help)
            echo "Usage: $0 [--default-server <host:port> | <host:port>]"
            exit 0
            ;;
        -*)
            echo "Error: unknown option: $1" >&2
            exit 1
            ;;
        *)
            if [[ -z "$CLI_DEFAULT_SERVER" ]]; then
                CLI_DEFAULT_SERVER="$1"
            else
                echo "Error: unexpected argument: $1" >&2
                exit 1
            fi
            shift
            ;;
    esac
done

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN_DIR="$ROOT_DIR/bin"
RELEASES_DIR="$ROOT_DIR/releases"
WIN7_TOOLCHAIN_DIR="$ROOT_DIR/.toolchain/go-win7"

mkdir -p "$BIN_DIR" "$RELEASES_DIR"

# Load .env file if present
ENV_FILE="$ROOT_DIR/.env"
if [[ -f "$ENV_FILE" ]]; then
    echo "==> Loading build configuration from .env..."
    while IFS= read -r raw_line || [[ -n "$raw_line" ]]; do
        line="${raw_line%$'\r'}"
        line="${line#"${line%%[![:space:]]*}"}"
        line="${line%"${line##*[![:space:]]}"}"
        [[ -z "$line" || "$line" == \#* || "$line" != *=* ]] && continue
        k="${line%%=*}"
        v="${line#*=}"
        k="${k%"${k##*[![:space:]]}"}"
        v="${v#"${v%%[![:space:]]*}"}"
        v="${v%"${v##*[![:space:]]}"}"
        if [[ "$v" == \"*\" && "$v" == *\" ]]; then
            v="${v:1:${#v}-2}"
        elif [[ "$v" == \'*\' && "$v" == *\' ]]; then
            v="${v:1:${#v}-2}"
        fi
        if [[ -n "$k" ]]; then
            export "$k=$v"
        fi
    done < "$ENV_FILE"
fi

if [[ -n "$CLI_DEFAULT_SERVER" ]]; then
    DEFAULT_SERVER="$CLI_DEFAULT_SERVER"
elif [[ -n "${DEFAULT_SERVER:-}" ]]; then
    :
elif [[ -n "${DEFAULT_SERVER_ADDR:-}" ]]; then
    DEFAULT_SERVER="$DEFAULT_SERVER_ADDR"
elif [[ -n "${MCP_SERVER_ADDR:-}" ]]; then
    DEFAULT_SERVER="$MCP_SERVER_ADDR"
else
    DEFAULT_SERVER=""
fi

LDFLAGS="-s -w"
if [[ -n "$DEFAULT_SERVER" ]]; then
    LDFLAGS="$LDFLAGS -X mcp-remoteaccess/internal/protocol.DefaultServerAddr=$DEFAULT_SERVER"
    echo "==> Embedding DefaultServerAddr: $DEFAULT_SERVER"
else
    echo "==> WARNING: DefaultServerAddr not specified (using 127.0.0.1:20055). Create .env or pass --default-server."
fi

echo "==> Building Windows (amd64) binaries with standard Go..."
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="$LDFLAGS" -o "$BIN_DIR/relay-server.exe" ./cmd/relay-server
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="$LDFLAGS" -o "$BIN_DIR/dev-client.exe" ./cmd/dev-client
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="$LDFLAGS -H=windowsgui" -o "$BIN_DIR/user-client.exe" ./cmd/user-client

echo "==> Building Linux (amd64 & arm64) binaries with standard Go..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$LDFLAGS" -o "$BIN_DIR/relay-server-linux-amd64" ./cmd/relay-server
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$LDFLAGS" -o "$BIN_DIR/dev-client-linux-amd64" ./cmd/dev-client
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$LDFLAGS" -o "$BIN_DIR/user-client-linux-amd64" ./cmd/user-client
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="$LDFLAGS" -o "$BIN_DIR/user-client-linux-arm64" ./cmd/user-client

# Detect host OS/arch for XTLS/go-win7 toolchain
uname_s="$(uname -s)"
uname_m="$(uname -m)"
case "$uname_s" in
    Linux*)                   HOST_OS="linux" ;;
    Darwin*)                  HOST_OS="darwin" ;;
    MINGW*|MSYS*|CYGWIN*)     HOST_OS="windows" ;;
    *)
        echo "Error: unsupported host OS for go-win7 download: $uname_s" >&2
        exit 1
        ;;
esac
case "$uname_m" in
    x86_64|amd64)             HOST_ARCH="amd64" ;;
    aarch64|arm64)            HOST_ARCH="arm64" ;;
    *)
        echo "Error: unsupported host architecture for go-win7 download: $uname_m" >&2
        exit 1
        ;;
esac

if [[ "$HOST_OS" == "windows" ]]; then
    WIN7_GO_BIN="$WIN7_TOOLCHAIN_DIR/bin/go.exe"
else
    WIN7_GO_BIN="$WIN7_TOOLCHAIN_DIR/bin/go"
fi

if [[ ! -x "$WIN7_GO_BIN" ]]; then
    ASSET_NAME="go-for-win7-${HOST_OS}-${HOST_ARCH}.zip"
    case "${HOST_OS}-${HOST_ARCH}" in
        linux-amd64)   EXPECTED_SHA="057979a62b77ce5104644b1ea413fdf41c1bed5d22523ad99d464fbf03d22f6d" ;;
        linux-arm64)   EXPECTED_SHA="f8402d8560b47a709ff0851aa0f41ecd0ec4294a86feea863ddafaab1be2663d" ;;
        darwin-amd64)  EXPECTED_SHA="cdd294072c1668624af044a4967465e1e069ec964a477c773b99700a8aec56cd" ;;
        darwin-arm64)  EXPECTED_SHA="2316ea18ac6dfd9b0d52eca1b6b44742d6d5c75583c07ff45df7a7e23794512c" ;;
        windows-amd64) EXPECTED_SHA="2fccdac532798af3e7ffe73a18e25f0763d22d4c87f963075c0f59fcef8e3645" ;;
        windows-arm64) EXPECTED_SHA="e6986a08b265c3893be47f95f6b6e40a7730045ab8eac2783117e85f3265605d" ;;
    esac
    ZIP_URL="https://github.com/XTLS/go-win7/releases/download/patched-1.27.4/${ASSET_NAME}"
    TEMP_ZIP="$(mktemp "${TMPDIR:-/tmp}/${ASSET_NAME}.XXXXXX")"
    trap 'rm -f "$TEMP_ZIP"' EXIT

    echo "==> Downloading XTLS/go-win7 toolchain (patched-1.27.4, ${HOST_OS}/${HOST_ARCH}) for windows7-i386 build..."
    mkdir -p "$WIN7_TOOLCHAIN_DIR"
    curl -fSL "$ZIP_URL" -o "$TEMP_ZIP"

    if command -v sha256sum >/dev/null 2>&1; then
        ACTUAL_SHA="$(sha256sum "$TEMP_ZIP" | awk '{print tolower($1)}')"
    elif command -v shasum >/dev/null 2>&1; then
        ACTUAL_SHA="$(shasum -a 256 "$TEMP_ZIP" | awk '{print tolower($1)}')"
    elif command -v openssl >/dev/null 2>&1; then
        ACTUAL_SHA="$(openssl dgst -sha256 "$TEMP_ZIP" | awk '{print tolower($NF)}')"
    else
        echo "Error: no sha256 tool found (sha256sum, shasum, or openssl)" >&2
        exit 1
    fi

    if [[ "$ACTUAL_SHA" != "$EXPECTED_SHA" ]]; then
        echo "Error: go-win7 SHA256 mismatch: expected $EXPECTED_SHA, got $ACTUAL_SHA" >&2
        exit 1
    fi

    if command -v unzip >/dev/null 2>&1; then
        unzip -oq "$TEMP_ZIP" -d "$WIN7_TOOLCHAIN_DIR"
    elif command -v tar >/dev/null 2>&1; then
        tar -xf "$TEMP_ZIP" -C "$WIN7_TOOLCHAIN_DIR"
    elif command -v python3 >/dev/null 2>&1; then
        python3 -m zipfile -e "$TEMP_ZIP" "$WIN7_TOOLCHAIN_DIR"
    else
        echo "Error: no zip extraction tool found (unzip, tar, or python3)" >&2
        exit 1
    fi
fi

echo "==> Building Windows (386 / Win7+ compatible) user-client with go-win7..."
GOROOT="$WIN7_TOOLCHAIN_DIR" GOTOOLCHAIN=local CGO_ENABLED=0 GOOS=windows GOARCH=386 \
    "$WIN7_GO_BIN" build -trimpath -ldflags="$LDFLAGS -H=windowsgui" -o "$BIN_DIR/user-client-windows-386.exe" ./cmd/user-client

echo "==> Populating ./releases/ for user-client auto-update..."
cp -f "$BIN_DIR/user-client.exe" "$RELEASES_DIR/user-client-windows-amd64.exe"
cp -f "$BIN_DIR/user-client-windows-386.exe" "$RELEASES_DIR/user-client-windows-386.exe"
cp -f "$BIN_DIR/user-client-linux-amd64" "$RELEASES_DIR/user-client-linux-amd64"
cp -f "$BIN_DIR/user-client-linux-arm64" "$RELEASES_DIR/user-client-linux-arm64"

echo "==> Build complete! Artifacts in ./bin and ./releases:"
ls -lh "$BIN_DIR" "$RELEASES_DIR"
