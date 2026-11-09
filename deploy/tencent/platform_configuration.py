#!/usr/bin/env python3
"""Apply the dedicated service-management write control to a staged release."""

import os
from pathlib import Path
import sys


def configure(filename, value):
    if not value:
        return
    if value not in ('true', 'false'):
        raise ValueError('PLATFORM_OPERATIONS_WRITES_ENABLED must be true or false')
    target = Path(filename)
    lines = [line for line in target.read_text().splitlines()
             if not line.strip().startswith('PLATFORM_OPERATIONS_WRITES_ENABLED=')]
    lines.append('PLATFORM_OPERATIONS_WRITES_ENABLED=' + value)
    target.write_text('\n'.join(lines) + '\n')


if __name__ == '__main__':
    configure(sys.argv[1], os.environ.get('PLATFORM_OPERATIONS_WRITES_ENABLED', ''))
