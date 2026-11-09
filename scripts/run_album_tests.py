"""Run every Albums test once across four isolated, bounded storage workers."""

import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import time


def groups(names, workers=4):
    if not names or len(names) != len(set(names)):
        raise ValueError('test inventory must be nonempty and unique')
    return [names[index::workers] for index in range(min(workers, len(names)))]


def main():
    if not all(os.environ.get(name) for name in ('MYSQL_TEST_DSN', 'MYSQL_TEST_ADMIN_DSN', 'REDIS_TEST_URL')):
        raise ValueError('real MySQL and Redis are required')
    root = Path(__file__).resolve().parent.parent
    with tempfile.TemporaryDirectory(prefix='albums-storage-', dir=os.getenv('RUNNER_TEMP')) as directory:
        temporary = Path(directory)
        binary = temporary / 'albums.test'
        subprocess.run(['go', 'test', '-c', '-o', str(binary), './internal/albums'], cwd=root, check=True)
        env = os.environ.copy()
        inventory = subprocess.check_output([str(binary), '-test.list=^Test'], cwd=root / 'internal/albums', env=env).decode()
        names = re.findall(r'^Test\S+$', inventory, re.MULTILINE)
        processes, logs = [], []
        started = time.monotonic()
        try:
            for index, selected in enumerate(groups(names)):
                log = (temporary / f'worker-{index}.log').open('w+')
                logs.append(log)
                process = subprocess.Popen([str(binary), '-test.v', '-test.timeout=30m', '-test.run=^(' + '|'.join(selected) + ')$'],
                                           cwd=root / 'internal/albums', env=env, stdout=log, stderr=subprocess.STDOUT)
                processes.append((process, selected))
            failed = False
            for index, ((process, selected), log) in enumerate(zip(processes, logs)):
                code = process.wait(timeout=1830)
                log.seek(0)
                output = log.read()
                completed = re.findall(r'^--- (PASS|SKIP): (Test[^ /]+) \(', output, re.MULTILINE)
                passed = {name for status, name in completed if status == 'PASS'}
                valid = (code == 0 and {name for _, name in completed} == set(selected)
                         and any(name.startswith('TestMySQL') for name in passed))
                print(json.dumps({'worker': index, 'tests': len(selected), 'passed': len(passed), 'success': valid}), flush=True)
                if not valid:
                    print(output, flush=True)
                    failed = True
            print(json.dumps({'total_tests': len(names), 'seconds': round(time.monotonic() - started, 3), 'passed': not failed}), flush=True)
            if failed:
                raise SystemExit(1)
        finally:
            for process, _ in processes:
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait()
            for log in logs:
                log.close()


if __name__ == '__main__':
    main()
