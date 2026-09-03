#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
backend_dir=$(CDPATH= cd -- "$script_dir/../.." && pwd)
exec python3 "$backend_dir/deploy/tencent/operations.py" backup --backend-dir "$backend_dir"
