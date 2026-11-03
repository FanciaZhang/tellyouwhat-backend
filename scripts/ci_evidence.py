"""Reuse successful verification of identical inputs from an ancestor's trusted run."""

import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import zipfile


SERVICES = ('gateway', 'worker', 'admin', 'adminctl', 'migrate', 'maintenance', 'albumgateway', 'albumworker')
COMMON = {'.github/workflows/backend.yml', 'scripts/ci_evidence.py', 'go.mod', 'go.sum'}


def git(*args):
    return subprocess.check_output(['git', *args], stderr=subprocess.DEVNULL)


def fingerprints(commit, image):
    records = git('ls-tree', '-rz', commit).split(b'\0')
    selected = {'full': [], 'albums': [], 'swift': []}
    for record in filter(None, records):
        path = record.split(b'\t', 1)[1].decode()
        selected['full'].append(record)
        # Albums recovery tests execute operations code; health polling is independent.
        if (path in COMMON or not (path.startswith(('docs/', '.github/')) or path.endswith('.md')
                                   or path in ('deploy/tencent/ops_health.py', 'deploy/tencent/health_test.py'))):
            selected['albums'].append(record)
        if path in COMMON or path.startswith('Contracts/'):
            selected['swift'].append(record)
    return {scope: hashlib.sha256(image.encode() + b'\0' + b'\0'.join(values)).hexdigest()
            for scope, values in selected.items()}


def api(path):
    return subprocess.check_output(['gh', 'api', path], stderr=subprocess.DEVNULL)


def receipt(repo, run):
    artifacts = json.loads(api(f'repos/{repo}/actions/runs/{run["id"]}/artifacts?per_page=100'))['artifacts']
    for artifact in sorted(artifacts, key=lambda a: a['id'], reverse=True):
        if artifact['expired'] or not artifact['name'].startswith('backend-verification-'):
            continue
        data = api(f'repos/{repo}/actions/artifacts/{artifact["id"]}/zip')
        with zipfile.ZipFile(io.BytesIO(data)) as archive:
            member = archive.getinfo('verification.json')
            if member.file_size > 16384:
                raise ValueError('verification receipt exceeds its bound')
            return json.loads(archive.read(member))
    return None


def select_evidence(repo, commit, image, runs, current_run):
    keys = fingerprints(commit, image)
    result = {scope: '' for scope in (*keys, 'images')}
    blocked = set()
    for run in runs:
        source = run.get('head_sha', '')
        if (str(run['id']) == str(current_run) or not re.fullmatch('[a-f0-9]{40}', source)
                or run.get('status') != 'completed' or (run.get('head_repository') or {}).get('full_name', '').lower() != repo.lower()
                or run.get('event') not in ('push', 'pull_request', 'workflow_dispatch')):
            continue
        try:
            # Unmerged or unrelated PR code cannot supply production verification.
            subprocess.run(['git', 'merge-base', '--is-ancestor', source, commit], check=True, capture_output=True)
            expected = fingerprints(source, image)
            matching = [scope for scope in keys if scope not in blocked and not result[scope] and keys[scope] == expected[scope]]
            scopes = matching
            trusted_main = run.get('head_branch') == 'main' and run['event'] != 'pull_request'
            if keys['full'] != expected['full'] and not trusted_main:
                scopes = []
            if not scopes and (result['images'] or source != commit) and not (
                    matching and run.get('conclusion') in ('failure', 'cancelled', 'timed_out')):
                continue
            jobs = json.loads(api(f'repos/{repo}/actions/runs/{run["id"]}/jobs?per_page=100'))['jobs']
            verification = next((job for job in jobs if job['name'] == 'verify'), {})
            if verification.get('conclusion') == 'failure':
                failed = [step['name'] for step in verification.get('steps', []) if step.get('conclusion') == 'failure']
                affected = {'Test Albums with real storage': {'full', 'albums'},
                            'Test with MySQL and Redis': {'full', 'albums'},
                            'Run swift test --package-path Contracts/HTTP': {'full', 'swift'}}
                for name in failed or ['unknown']:
                    blocked.update(affected.get(name, set(keys)) & set(matching))
                continue
            if verification.get('conclusion') != 'success':
                continue
            if trusted_main and run['event'] == 'workflow_dispatch':
                trusted_main = any(step.get('name') == 'Log in to GHCR' and step.get('conclusion') == 'success'
                                   for job in jobs for step in job.get('steps', []))
                if not trusted_main and keys['full'] != expected['full']:
                    scopes = []
            record = receipt(repo, run)
            if (not record or record.get('version') != 1 or record.get('commit') != source
                    or record.get('image') != image or record.get('keys') != expected):
                continue
            for scope in scopes:
                result[scope] = str(run['id'])
            published = trusted_main
            # Non-release checks reuse build results; production requires published main images.
            if (source == commit and (published or os.getenv('GITHUB_REF') != 'refs/heads/main')
                    and {f'container ({name})' for name in SERVICES}.issubset(
                        job['name'] for job in jobs if job['conclusion'] == 'success')):
                result['images'] = str(run['id'])
            if all(result.values()):
                break
        except (subprocess.CalledProcessError, KeyError, OSError, ValueError, zipfile.BadZipFile):
            continue
    return result


def main():
    commit = git('rev-parse', 'HEAD').decode().strip()
    image = os.getenv('ImageOS', '') + ':' + os.getenv('ImageVersion', '')
    if sys.argv[1] == 'record':
        if git('status', '--porcelain', '--untracked-files=no').strip():
            raise SystemExit('Verification changed tracked source; refusing a receipt.')
        Path(sys.argv[2]).write_text(json.dumps({'version': 1, 'commit': commit, 'image': image,
                                               'reused_from': {scope: os.getenv('CI_REUSED_' + scope.upper(), '') for scope in ('full', 'albums', 'swift')},
                                               'keys': fingerprints(commit, image)}, sort_keys=True) + '\n')
        return
    base = os.getenv('PR_BASE_SHA')
    if base and (not re.fullmatch('[a-f0-9]{40}', base) or subprocess.run(
            ['git', 'merge-base', '--is-ancestor', base, commit], capture_output=True).returncode):
        raise SystemExit('Update the PR with its current base before verification.')
    result = {scope: '' for scope in ('full', 'albums', 'swift', 'images')}
    try:
        if int(os.getenv('GITHUB_RUN_ATTEMPT', '1')) > 1:
            raise ValueError('verification retry requires fresh checks')
        if not os.getenv('ImageVersion'):
            raise ValueError('runner image identity is unavailable')
        repo = os.environ['GITHUB_REPOSITORY']
        runs = json.loads(api(f'repos/{repo}/actions/workflows/backend.yml/runs?per_page=30'))['workflow_runs']
        result = select_evidence(repo, commit, image, runs, os.environ['GITHUB_RUN_ID'])
    except (subprocess.CalledProcessError, KeyError, OSError, ValueError):
        print('Prior verification could not be authenticated; running the checks.')
    with open(os.environ['GITHUB_OUTPUT'], 'a') as output:
        for key, value in result.items():
            output.write(f'{key}={value}\n')
    print(json.dumps({'verification_source_runs': result}, sort_keys=True))


if __name__ == '__main__':
    main()
