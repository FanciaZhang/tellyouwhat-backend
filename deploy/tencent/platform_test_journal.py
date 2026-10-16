"""Admit the already provisioned Journal development resources to the shared gateway."""

import re

from ops_common import OperationError, read_environment

HOST = 'api-test.journal.tellyouwhat.cn'
FIXED = {'JOURNAL_APPLE_BUNDLE_ID': 'cn.tellyouwhat.journalapp.dev', 'JOURNAL_APPLE_TEAM_ID': 'T6DD96HY2B',
         'JOURNAL_API_HOSTS': HOST, 'JOURNAL_APP_ATTEST_ENV': 'production',
         'JOURNAL_ARK_BASE_URL': 'https://ark.cn-beijing.volces.com/api/v3',
         'JOURNAL_IMAGE_BASE_URL': 'https://ark.cn-beijing.volces.com/api/v3',
         'JOURNAL_VOICE_ENABLED': 'true', 'JOURNAL_IMAGE_ENABLED': 'true'}
RESOURCE_KEYS = {'JOURNAL_ARK_API_KEY': 'JOURNAL_ARK_API_KEY',
                 'JOURNAL_ARK_LITE_MODEL_ID': 'JOURNAL_ARK_LITE_MODEL',
                 'JOURNAL_ARK_PRO_MODEL_ID': 'JOURNAL_ARK_PRO_MODEL',
                 'JOURNAL_VOICE_MODEL_ID': 'JOURNAL_VOICE_MODEL',
                 'JOURNAL_SPEECH_API_KEY': 'JOURNAL_VOICE_ASR_API_KEY',
                 'JOURNAL_IMAGE_API_KEY': 'JOURNAL_IMAGE_API_KEY',
                 'JOURNAL_IMAGE_MODEL_ID': 'JOURNAL_IMAGE_MODEL_ID',
                 'JOURNAL_IMAGE_MAX_COST_NANOS': 'JOURNAL_IMAGE_MAX_COST_NANOS'}
SETTINGS = set(FIXED) | set(RESOURCE_KEYS) | {'JOURNAL_ALLOWED_APP_BUILDS', 'JOURNAL_RECORDING_ENABLED'}


def validate(app):
    if (any(app.get(k) != v for k, v in FIXED.items())
            or app.get('JOURNAL_RECORDING_ENABLED') not in ('false', 'true')
            or not re.fullmatch(r'\d+(,\d+)*', app.get('JOURNAL_ALLOWED_APP_BUILDS', ''))
            or any(not app.get(k) for k in RESOURCE_KEYS)
            or any(k.startswith('JOURNAL_') and k not in SETTINGS for k in app)
            or any(not re.fullmatch(r'ep-[a-z0-9-]+', app.get(k, '')) for k in RESOURCE_KEYS if k.endswith('_MODEL_ID'))
            or not re.fullmatch(r'\d{1,10}', app.get('JOURNAL_IMAGE_MAX_COST_NANOS', ''))):
        raise OperationError('Journal test identity, inference or resource boundary is invalid')
    if not 0 < int(app['JOURNAL_IMAGE_MAX_COST_NANOS']) <= 1_000_000_000:
        raise OperationError('Journal test image price ceiling is invalid')


def require_resources(runtime, app):
    from platform_test_deploy import private_file
    path = runtime.root.parent / 'journal-development/runtime.env'
    private_file(runtime, path, 32768)
    approved = read_environment(path)
    for key, source in RESOURCE_KEYS.items():
        if not approved.get(source) or app[key] != approved[source]:
            raise OperationError('Journal shared test must use its existing restricted development resources')
        if key != 'JOURNAL_IMAGE_MAX_COST_NANOS' and app[key] == runtime.config.get(key):
            raise OperationError('Journal test credentials and endpoints must differ from production')
