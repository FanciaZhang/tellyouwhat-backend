"""Reviewed forward-only test migrations whose old APIs can still run safely."""

import hashlib
from pathlib import Path
import re
import tarfile

from ops_common import OperationError

# Exact reviewed changes preserve existing data and the previous API. Do not infer safety from SQL keywords.
ADDITIONS = {'migrations/0065_journal_illustration_jobs.sql':
             '7788905018d96b960c109bad7a810fdfa503486c47975dbc59287811437c5ca6',
             'migrations/0066_verified_entitlement_environments.sql': '04b74a306bb945b495a7b842207a95234ea84d9d2f0df3f9d6090653ebeb00e9',
             'migrations/0067_journal_illustration_consent_scope.sql': '8e1ba4642191b15c8a5efe7891e6c445679ff38d9b49799bfce613baafff22cf',
             'migrations/0068_journal_read_aloud_quota.sql': '0133b95c95ae8d7297e8bc507d4f8aad9eff31089e4c1a2529ce55379533f7e0',
             'migrations/0069_journal_voice_catalog.sql': '3cc56f88627e5f31f952891e9ecc4bc829fb208d57d8f2d031a406cd60459c60',
             'migrations/0070_journal_read_aloud_consent.sql': '4b26bac5e6ef50e28c2284c888e077fb2fe7bff3ccef358daef4fd3a4e2e0a95',
             'migrations/0071_journal_simulator_read_aloud_consents.sql': '968c1b49952a41965312e9c47c88c08dc9c95affe5ea5d9a90dafb7b6f2b4631',
             'migrations/0072_health_guardian_consent_scope.sql': '987e13cd0bb2c6e06f3c9ef34349ff277dd58738d14cbe61724cc38982943574',
             'migrations/0073_album_development_subscription_products.sql': 'b2b984c582cb774881f06166716d600852da07b7b646e77c888744c01990e5f7',
             'migrations/0074_album_system_copy_locators.sql': 'd700b814fd9d1dcc8f3014f60db6677cc641cfa35faa6c7d2c3de4d149e16a0d'}


def hashes(folder):
    with tarfile.open(folder / 'source.tar') as archive:
        members = [m for m in archive.getmembers() if m.isfile() and re.fullmatch(r'migrations/\d{4}_[^/]+\.sql', m.name)]
        if len({m.name for m in members}) != len(members):
            raise OperationError('duplicate database schemas in selected source')
        return {m.name: hashlib.sha256(archive.extractfile(m).read()).hexdigest() for m in members}


def plan(before, after):
    old, new = hashes(before), hashes(after)
    additions = {k: v for k, v in new.items() if k not in old}
    if (not old or any(new.get(k) != v for k, v in old.items())
            or any(ADDITIONS.get(k) != v or k <= max(old) for k, v in additions.items())):
        raise OperationError('unreviewed database schemas cannot be migrated or rolled back')
    return sorted(Path(k).name for k in old), sorted(Path(k).name for k in new)


def applied(runtime, project):
    return runtime.execute('platform-test-migration-names', [
        'docker', 'exec', project + '-mysql-1', 'sh', '-c',
        'MYSQL_PWD="$MYSQL_PASSWORD" exec mysql --user="$MYSQL_USER" --database=platform_test '
        '--batch --skip-column-names -e "SELECT name FROM schema_migrations ORDER BY name;"']).decode().splitlines()
