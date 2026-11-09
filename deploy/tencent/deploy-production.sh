#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 3 || $# -gt 4 || ( "$3" != internal && "$3" != public ) ]]; then
  echo "usage: $0 <image-tag> <image-registry-prefix> <internal|public> [staged-release-directory]" >&2
  exit 64
fi

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
arguments=(deploy --tag "$1" --registry "$2" --acceptance "$3")
if [[ $# == 4 ]]; then arguments+=(--bundle "$4"); fi
exec python3 "$script_dir/release.py" "${arguments[@]}"
