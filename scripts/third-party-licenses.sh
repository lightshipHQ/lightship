#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo 'Usage: bash scripts/third-party-licenses.sh OUTPUT_DIRECTORY' >&2
  exit 2
fi

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_root"
output_dir=$1
go mod download

# go-licenses does not recognize MIT-0; this exception is bound to the reviewed version and text.
test "$(go list -m -f '{{.Version}}' github.com/segmentio/asm)" = v1.2.1
asm_license="$(go list -m -f '{{.Dir}}' github.com/segmentio/asm)/LICENSE"
expected_checksum=cca993712df289a5958bdef69031a5dac0f951ac15afeb313f9eeea55ed59443
if command -v sha256sum >/dev/null 2>&1; then
  actual_checksum=$(sha256sum "$asm_license")
else
  actual_checksum=$(shasum -a 256 "$asm_license")
fi
test "${actual_checksum%% *}" = "$expected_checksum"

go run github.com/google/go-licenses/v2@v2.0.1 save ./cmd/lightship \
  --ignore github.com/segmentio/asm --save_path "$output_dir"
mkdir -p "$output_dir/github.com/segmentio/asm" "$output_dir/go"
cp "$asm_license" "$output_dir/github.com/segmentio/asm/LICENSE"
cp "$(go env GOROOT)/LICENSE" "$output_dir/go/LICENSE"
mkdir -p "$output_dir/ui"
cp internal/httpapi/ui/primer-LICENSES.txt "$output_dir/ui/primer-LICENSES.txt"
cp internal/httpapi/ui/instrument-sans-OFL.txt "$output_dir/ui/instrument-sans-OFL.txt"
cp internal/httpapi/ui/geist-mono-OFL.txt "$output_dir/ui/geist-mono-OFL.txt"
