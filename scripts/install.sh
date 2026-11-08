#!/bin/sh
set -eu

BASE="${CINDERLINE_DOWNLOAD_URL:-https://github.com/rodo-software/cinderline-runner/releases/latest/download}"

case "$(uname -s)" in
Linux)  OS=linux  ;;
Darwin) OS=darwin ;;
*)
	echo "Unsupported OS: $(uname -s); expected Linux or Darwin" >&2
	exit 1
	;;
esac

case "$(uname -m)" in
x86_64|amd64)  ARCH=amd64 ;;
arm64|aarch64) ARCH=arm64 ;;
*)
	echo "Unsupported architecture: $(uname -m); expected x86_64, amd64, arm64 or aarch64" >&2
	exit 1
	;;
esac

FILE="cinderline_${OS}_${ARCH}.tar.gz"

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

curl -fsSL -o "$TMPDIR/$FILE" "$BASE/$FILE"
curl -fsSL -o "$TMPDIR/checksums.txt" "$BASE/checksums.txt"

EXPECTED=$(awk '$2 == "'"$FILE"'" { print $1 }' "$TMPDIR/checksums.txt")

if [ -z "$EXPECTED" ]; then
	echo "checksum: no checksum found for $FILE in checksums.txt; refusing to install" >&2
	exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
	ACTUAL=$(sha256sum "$TMPDIR/$FILE" | awk '{print $1}')
else
	ACTUAL=$(shasum -a 256 "$TMPDIR/$FILE" | awk '{print $1}')
fi

if [ "$ACTUAL" != "$EXPECTED" ]; then
	echo "checksum mismatch for $FILE: refusing to install" >&2
	exit 1
fi

tar -xzf "$TMPDIR/$FILE" -C "$TMPDIR"
mkdir -p "$HOME/.local/bin"
cp "$TMPDIR/cinderline" "$HOME/.local/bin/.cinderline.tmp"
chmod 755 "$HOME/.local/bin/.cinderline.tmp"
mv "$HOME/.local/bin/.cinderline.tmp" "$HOME/.local/bin/cinderline"

echo "Installed cinderline to $HOME/.local/bin/cinderline"

case ":${PATH:-}:" in
*:"$HOME/.local/bin":*) ;;
*)
	echo "Warning: $HOME/.local/bin is not in your PATH."
	echo "Add this line to your ~/.profile, ~/.bashrc or ~/.zshrc:"
	echo
	echo '  export PATH="$HOME/.local/bin:$PATH"'
	;;
esac

echo
echo "To check the build provenance: gh attestation verify $FILE --repo rodo-software/cinderline-runner"
echo "(Download the archive first with: curl -fsSL -o \"$FILE\" \"$BASE/$FILE\")"
