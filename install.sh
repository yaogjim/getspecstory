#!/bin/bash
set -e

# Release source. Defaults to this fork so installs and updates stay on the fork's
# own releases (which carry the Cursor resume fix); set SPECSTORY_REPO to override.
REPO="${SPECSTORY_REPO:-yaogjim/getspecstory}"
BINARY_NAME="specstory"
INSTALL_DIR="/usr/local/bin"

# Detect platform
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

case $ARCH in
    x86_64) ARCH="x86_64" ;;
    arm64|aarch64) ARCH="arm64" ;;
    *) echo "❌ Unsupported architecture: $ARCH"; exit 1 ;;
esac

case $OS in
    darwin) OS="Darwin" ;;
    linux) OS="Linux" ;;
    *) echo "❌ Unsupported OS: $OS"; exit 1 ;;
esac

echo "🚀 Installing SpecStory CLI for $OS $ARCH..."

# Get latest version. Read it from the releases/latest redirect rather than the
# GitHub API: the unauthenticated API is limited to 60 requests/hour per IP,
# which makes installs fail on shared or busy networks.
VERSION=$(curl -sI "https://github.com/$REPO/releases/latest" \
    | tr -d '\r' \
    | sed -nE 's#^[Ll]ocation: .*/releases/tag/([^/[:space:]]+)$#\1#p')

if [ -z "$VERSION" ]; then
    echo "❌ Failed to get latest version"
    exit 1
fi

# Download and install
FILENAME="SpecStoryCLI_${OS}_${ARCH}.tar.gz"
DOWNLOAD_URL="https://github.com/$REPO/releases/download/$VERSION/$FILENAME"
TMP_DIR=$(mktemp -d)

echo "📥 Downloading $VERSION..."
curl -sL "$DOWNLOAD_URL" | tar -xz -C "$TMP_DIR"

# Install with sudo if needed
if [ -w "$INSTALL_DIR" ]; then
    mv "$TMP_DIR/$BINARY_NAME" "$INSTALL_DIR/"
else
    echo "🔐 Installing to $INSTALL_DIR (requires sudo)..."
    sudo mv "$TMP_DIR/$BINARY_NAME" "$INSTALL_DIR/"
fi

chmod +x "$INSTALL_DIR/$BINARY_NAME"
rm -rf "$TMP_DIR"

echo "✅ SpecStory installed successfully!"
echo "Try: $BINARY_NAME --version"
