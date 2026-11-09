"""Run the selected test maintenance image under the existing host operations lock."""

import json

from ops_common import OperationError
from platform_test_deploy import compose, private_file, selection
from platform_test_storage import available


def maintain(runtime):
    state_path = runtime.state / 'platform-test.json'
    if not state_path.exists():
        return {'configured': False}
    state = json.loads(private_file(runtime, state_path, 16384))
    folder, _, _ = selection(runtime, state.get('source'))
    owner = json.loads(private_file(runtime, runtime.state / 'artifact-owners/platform-test.json', 16384))
    current = owner.get('current', {})
    if (state.get('release') != str(folder) or current.get('source') != state['source']
            or str(folder) not in current.get('paths', [])):
        raise OperationError('platform test maintenance must match the registered active release')
    if not available(folder):
        return {'configured': False, 'reason': 'selected release has no maintenance service'}
    # Retention must still run when the API is stopped or generation is disabled.
    runtime.execute('platform-test-maintenance', compose(folder, 'run', '--rm', '--no-deps', 'maintenance'), timeout=900)
    return runtime.record('platform-test-maintenance', configured=True, source=state['source'], passed=True)
