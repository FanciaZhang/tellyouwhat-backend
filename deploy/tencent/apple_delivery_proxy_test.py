"""Check capability URLs against real proxy error logging without touching active Caddy."""

from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import time
import unittest
import urllib.error
import urllib.request


HERE = Path(__file__).resolve().parent


def unused_port():
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        return listener.getsockname()[1]


class AppleDeliveryProxyTests(unittest.TestCase):
    @unittest.skipUnless(shutil.which('caddy'), 'native Caddy is required for proxy logging acceptance')
    def test_error_log_exclusion_removes_only_installation_requests(self):
        protected = (HERE / 'apple-delivery-logging.caddy').read_text()
        fragment = (HERE / 'apple-delivery.caddy').read_text()
        for enabled in (False, True):
            with self.subTest(protection=enabled), tempfile.TemporaryDirectory(prefix='apple-delivery-proxy-') as directory:
                folder = Path(directory)
                port, unavailable = unused_port(), unused_port()
                self.assertNotEqual(port, unavailable)
                site = fragment.replace('install.tellyouwhat.cn {',
                                        f'http://install.example:{port} {{\n    bind 127.0.0.1')
                site = site.replace('127.0.0.1:18791', f'127.0.0.1:{unavailable}')
                global_options = protected if enabled else ''
                config = ('{\n    admin off\n    auto_https off\n    persist_config off\n' + global_options + '}\n' + site
                          + f'''\nhttp://control.example:{port} {{
    bind 127.0.0.1
    log control {{
        output discard
    }}
    reverse_proxy 127.0.0.1:{unavailable}
}}
''')
                source = folder / 'Caddyfile'
                source.write_text(config)
                tokens = {host: 'synthetic-installation-capability-' + str(index) for index, host in enumerate(
                    ('install.example', 'INSTALL.EXAMPLE', 'install.example:' + str(port)))}
                control = 'synthetic-control-request-for-log-test'
                opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

                def request(host, path):
                    try:
                        with opener.open(urllib.request.Request(f'http://127.0.0.1:{port}{path}', headers={'Host': host}), timeout=2) as response:
                            return response.status
                    except urllib.error.HTTPError as error:
                        error.close()
                        return error.code

                with (folder / 'proxy.log').open('wb') as output:
                    process = subprocess.Popen(['caddy', 'run', '--config', str(source), '--adapter', 'caddyfile'],
                                               stdout=output, stderr=output, cwd=folder)
                    try:
                        for attempt in range(60):
                            if process.poll() is not None:
                                self.fail('temporary Caddy exited: ' + (folder / 'proxy.log').read_text())
                            try:
                                if request('install.example', '/readyz') == 404:
                                    break
                            except OSError:
                                time.sleep(0.05)
                        else:
                            self.fail('temporary Caddy did not become ready')
                        for host, token in tokens.items():
                            self.assertEqual(request(host, '/d/' + token + '/app.ipa'), 502)
                        self.assertEqual(request('control.example', '/' + control), 502)
                    finally:
                        process.terminate()
                        try:
                            process.wait(timeout=5)
                        except subprocess.TimeoutExpired:
                            process.kill()
                            process.wait(timeout=5)
                logs = (folder / 'proxy.log').read_text()
                self.assertIn(control, logs, 'other sites must retain their error logs')
                for token in tokens.values():
                    self.assertEqual(token in logs, not enabled, 'installation capability leaked or negative control failed')


if __name__ == '__main__':
    unittest.main()
