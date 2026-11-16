#!/usr/bin/env python3
"""Apply dedicated operations settings to a staged release."""

import os
from pathlib import Path
import sys


def configure(filename, value, journal_tts_monthly_characters=''):
    if not value and not journal_tts_monthly_characters:
        return
    if value and value not in ('true', 'false'):
        raise ValueError('PLATFORM_OPERATIONS_WRITES_ENABLED must be true or false')
    if journal_tts_monthly_characters and (not journal_tts_monthly_characters.isascii()
            or not journal_tts_monthly_characters.isdecimal()
            or not 1 <= int(journal_tts_monthly_characters) <= 100_000_000):
        raise ValueError('JOURNAL_TTS_MONTHLY_CHARACTERS must be a bounded positive integer')
    updates = {}
    if value:
        updates['PLATFORM_OPERATIONS_WRITES_ENABLED'] = value
    if journal_tts_monthly_characters:
        updates['JOURNAL_TTS_MONTHLY_CHARACTERS'] = journal_tts_monthly_characters
    target = Path(filename)
    lines = [line for line in target.read_text().splitlines()
             if line.strip().partition('=')[0] not in updates]
    lines.extend(key + '=' + setting for key, setting in updates.items())
    target.write_text('\n'.join(lines) + '\n')


if __name__ == '__main__':
    configure(sys.argv[1], os.environ.get('PLATFORM_OPERATIONS_WRITES_ENABLED', ''),
              os.environ.get('JOURNAL_TTS_MONTHLY_CHARACTERS', ''))
