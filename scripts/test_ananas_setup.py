import json
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile
import shlex
import unittest
from unittest.mock import Mock, patch

import ananas_setup as setup
import ananas_setup_system as system


class SetupTests(unittest.TestCase):
    def test_only_qnap_device_responses_are_accepted(self):
        good = b'<QDocRoot version="1.0"><hostname>My QNAP</hostname><webAccessPort>8080</webAccessPort><doQuick/></QDocRoot>'
        self.assertEqual(setup.qnap_identity(good), 'My QNAP')
        for response in (b'<html>router</html>', b'<QDocRoot/>', b'<device><hostname>PC</hostname></device>', b'not xml', b'x' * 65537,
                         b'<!DOCTYPE QDocRoot><QDocRoot/>'):
            self.assertIsNone(setup.qnap_identity(response))

    def test_readable_connection_errors_do_not_repeat_diagnostics(self):
        for diagnostic, explanation in [('Connection refused', 'Enable SSH'), ('Permission denied', 'username and password'),
                                        ('Connection timed out', 'same LAN'), ('Host key verification failed', 'identity')]:
            message = setup.command_error('ssh', diagnostic.lower() + ' private-secret-detail', 255)
            self.assertIn(explanation, message)
            self.assertNotIn('private-secret-detail', message)
            self.assertNotIn('255', message)

    def test_configured_roots_are_first_but_other_folders_remain(self):
        managed = [dict(root='/share/volume/Public/Team', peers=['10.23.42.5'])]
        folders = [dict(name='Aardvark', path='/share/volume/Aardvark'), dict(name='Public', path='/share/volume/Public')]
        self.assertEqual([item['name'] for item in sorted(folders, key=lambda item: setup.folder_order(item, managed))], ['Public', 'Aardvark'])
        self.assertIn('Configured with anaNAS', setup.sync_tag('/share/volume/Public/Team', managed))
        self.assertIn('Contains', setup.sync_tag('/share/volume/Public', managed))
        self.assertIn('Not configured', setup.sync_tag('/share/volume/Aardvark', managed))
        roots = setup.browser_roots(dict(managed=managed, shares=folders))
        self.assertEqual([item['name'] for item in roots], ['Public/Team', 'Public', 'Aardvark'])
        self.assertEqual(roots[0]['relative'], 'Team')

    def test_browse_is_bounded_and_preserves_names(self):
        session = object.__new__(setup.Session)
        share = dict(name='Public', path='/share/volume/Public')
        session.shares = [share]
        session.command = Mock()
        # Build NUL records explicitly to avoid octal escape ambiguity.
        session.command.return_value = '\0'.join(['directory', 'My papers', '0', 'file', 'a b.txt', '12', 'more', '', '', ''])
        result = session.browse(share)
        self.assertEqual(result['entries'][0]['relative'], 'My papers')
        self.assertEqual(result['entries'][1]['bytes'], 12)
        self.assertTrue(result['truncated'])
        command = session.command.call_args.args[0]
        self.assertIn('1000', command)
        self.assertIn('pwd -P', command)
        subprocess.run(['/bin/sh', '-n'], input=command.encode(), check=True)
        for invalid in ('../secret', '/etc', 'A/../B', 'A//B'):
            with self.assertRaises(setup.SetupError):
                session.browse(share, invalid)
        with self.assertRaises(setup.SetupError):
            session.browse(dict(name='Unknown', path='/etc'))

    def test_identifier_rejects_shell_and_path_syntax(self):
        for bad in ("../bad", "", "x/y", "x\ny", "$(command)", "-option", "a" * 65):
            with self.subTest(bad=bad), self.assertRaises(setup.SetupError):
                setup.identifier(bad)
        self.assertEqual(setup.identifier("SampleDocuments_2026-09"), "SampleDocuments_2026-09")

    def test_rejects_remote_and_invalid_addresses_before_running_commands(self):
        with patch.object(setup, 'run') as command:
            for host in ('8.8.8.8', '127.0.0.1', '169.254.1.3', 'qnap.example', '0.0.0.0', '::1'):
                with self.assertRaises(setup.SetupError):
                    setup.lan(host)
            command.assert_not_called()

    def test_rejects_routed_nas(self):
        with patch.object(setup, 'run', return_value=b'[{"dev":"enp1s0","gateway":"10.23.42.1"}]'):
            with self.assertRaisesRegex(setup.SetupError, 'direct LAN'):
                setup.lan('192.168.2.30')

    def test_configured_profiles_are_preserved_and_deduplicated(self):
        with tempfile.TemporaryDirectory() as temporary, patch.object(setup, 'config_home', return_value=Path(temporary)):
            base = Path(temporary)
            profile = dict(local=dict(root='/tmp/local'), nas=dict(host='10.23.42.30'), stateDir='/tmp/state', webPort=8721)
            raw = json.dumps(profile).encode()
            setup.private_write(base / 'config.json', raw)
            directory = base / 'profiles' / '1234'
            directory.mkdir(parents=True)
            setup.private_write(directory / 'config.json', raw)
            found = setup.profiles()
            self.assertEqual(len(found), 1)
            self.assertEqual(found[0]['_service'], 'ananas-sync-1234.service')
            self.assertEqual((base / 'config.json').read_bytes(), raw)

    def test_local_root_rejects_overlap_and_nonempty(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            existing = [{'local': {'root': str(root / 'sync')}}]
            for value in (root, root / 'sync', root / 'sync/child'):
                with self.assertRaises(setup.SetupError):
                    setup.validate_local(str(value), existing)
            self.assertEqual(setup.validate_local(str(root / 'new'), existing), root / 'new')
            (root / 'full').mkdir()
            setup.private_write(root / 'full/file', b'keep')
            with self.assertRaises(setup.SetupError):
                setup.validate_local(str(root / 'full'), [])
            (root / 'alias').symlink_to(root / 'full')
            with self.assertRaises(setup.SetupError):
                setup.validate_local(str(root / 'alias'), [])

    def test_exclusive_private_write_never_overwrites(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / 'secret'
            setup.private_write(path, b'original')
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            with self.assertRaises(FileExistsError):
                setup.private_write(path, b'replacement')
            self.assertEqual(path.read_bytes(), b'original')

    def test_errors_do_not_expose_command_output_or_password(self):
        with patch.object(subprocess, 'run', return_value=Mock(returncode=1, stdout=b'password=secret', stderr=b'secret')):
            with self.assertRaises(setup.SetupError) as error:
                setup.run(['ssh', 'target'], data=b'secret')
            self.assertNotIn('secret', str(error.exception))

    def test_plan_refuses_managed_or_overlapping_nas_roots(self):
        session = Mock(host='10.23.42.30', route={})
        share = dict(name='Public', path='/share/Volume/Public')
        account = dict(name='user', uid=1000, gid=100)
        inventory = dict(shares=[share], accounts=[account], managed=[dict(root=share['path'] + '/existing', port=18742)])
        for folder in ('', 'existing'):
            with self.assertRaisesRegex(setup.SetupError, 'already managed'):
                setup.make_plan(session, inventory, share, folder, '/tmp/unused', account)

    def test_service_escapes_systemd_expansions(self):
        text = setup.service_text('/tmp/a $d%f/daemon', '/tmp/a "config"')
        self.assertIn('$$d%%f', text)
        self.assertIn('\\"config\\"', text)
        self.assertIn('Restart=on-failure', text)
        self.assertNotIn('shell', text)

    def test_certificates_match_host_and_remain_private(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary)
            pins = setup.certificates(path, '10.23.42.30', '10.23.42.17')
            self.assertEqual(len(pins['server']), 64)
            self.assertNotEqual(pins['server'], pins['client'])
            subprocess.run(['openssl', 'verify', '-CAfile', str(path / 'ca.pem'), '-verify_ip', '10.23.42.30', str(path / 'server.pem')], check=True, capture_output=True)
            self.assertTrue(all(item.stat().st_mode & 0o777 == 0o600 for item in path.iterdir()))

    def test_system_validator_refuses_injection(self):
        plan = dict(id='a' * 16, uid=os.getuid(), gid=os.getgid(), username=__import__('pwd').getpwuid(os.getuid()).pw_name,
                    host='10.23.42.30', route=dict(host='10.23.42.30', source='10.23.42.17', prefix='10.23.42.0/24', interface='enp1s0'),
                    share='Public', mountPoint='/mnt/ananas-' + 'a' * 16, port=18742)
        system.validate(plan)
        for key, value in [('id', '../etc'), ('mountPoint', '/'), ('share', 'Public\npassword=x'), ('port', 22)]:
            with self.subTest(key=key), self.assertRaises(ValueError):
                system.validate(dict(plan, **{key: value}))

    def test_known_host_never_trusts_new_unverified_key(self):
        route = dict(host='10.23.42.30', source='10.23.42.17', interface='enp1s0', prefix='10.23.42.0/24')
        old = b'10.23.42.30 ssh-rsa ORIGINAL\n'
        scanned = old + b'10.23.42.30 ssh-ed25519 UNTRUSTED\n'
        with tempfile.TemporaryDirectory() as temporary, patch.object(setup, 'config_home', return_value=Path(temporary)), patch.object(setup, 'lan', return_value=route):
            setup.private_write(Path(temporary) / 'known_hosts', old)
            session = setup.Session(route['host'], 'admin', 'fake-test-secret')
            try:
                with patch.object(setup, 'run', side_effect=[scanned, b'fingerprint', old]):
                    self.assertIsNone(session.host_key())
                self.assertEqual((session.directory / 'hostkey').read_bytes(), old)
            finally:
                session.close()

    def test_login_failure_removes_temporary_password(self):
        route = dict(host='10.23.42.30', source='10.23.42.17', interface='enp1s0', prefix='10.23.42.0/24')
        with patch.object(setup, 'lan', return_value=route):
            session = setup.Session(route['host'], 'admin', 'fake-test-secret')
            try:
                with patch.object(setup, 'run', side_effect=setup.SetupError('failed')) as command:
                    with self.assertRaises(setup.SetupError):
                        session.connect()
                    self.assertNotIn('fake-test-secret', repr(command.call_args))
                self.assertFalse((session.directory / 'password').exists())
                self.assertFalse((session.directory / 'askpass').exists())
            finally:
                session.close()

    def test_complete_new_instance_transaction_with_simulated_nas_and_system(self):
        """Real private files/certificates; no production writes or fake live claim."""
        route = dict(host='10.23.42.30', source='10.23.42.17', interface='enp1s0', prefix='10.23.42.0/24')
        payloads, calls, phases = {}, [], []
        session = Mock(host=route['host'], route=route, username='admin', password='fake-install-secret')
        share = dict(name='Public', path='/share/Volume/Public')
        account = dict(name='researcher', uid=1000, gid=100)
        inventory = dict(shares=[share], accounts=[account], managed=[], architecture='aarch64')
        def remote(command, **kwargs):
            calls.append(command)
            if command.startswith('cd ') and 'pwd -P' in command:
                return shlex.split(command)[1]
            if 'route get' in command:
                return '10.23.42.17 dev eth0 src 10.23.42.30'
            if 'data' in kwargs:
                tokens = shlex.split(command)
                target = tokens[tokens.index('>') + 1].rstrip(';')
                payloads[target] = kwargs['data']
            if command.startswith('sha256sum '):
                target = shlex.split(command)[1]
                return hashlib.sha256(payloads[target]).hexdigest() + '  ' + target
            return ''
        session.command.side_effect = remote
        original_run = setup.run
        privileged = []
        def local(argv, **kwargs):
            if argv[0] == 'openssl':
                return original_run(argv, **kwargs)
            self.assertNotIn(session.password, repr(argv))
            if '-client-identity' in argv:
                return json.dumps(dict(clientID='a' * 64)).encode()
            if argv[0] == 'pkexec':
                privileged.append(json.loads(kwargs['data']))
            return b''
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            with patch.object(Path, 'home', return_value=home), patch.object(setup, 'config_home', return_value=home / '.config/nas-sync'), \
                    patch.object(setup, 'lan', return_value=route), patch.object(setup, 'run', side_effect=local), \
                    patch.object(setup.time, 'sleep'), patch('nas_sync_indicator.Client') as client:
                binaries = setup.bundle_home() / 'bin/arm64'
                binaries.mkdir(parents=True)
                for name in ('helper', 'launcher'):
                    setup.private_write(binaries / name, b'test-fixture-binary')
                client.return_value.status.return_value = dict(ready=True, automaticWrites=True, syncReason='LAN sync active')
                plan = setup.make_plan(session, inventory, share, 'new-folder', str(home / 'files'), account)
                result = setup.install(session, inventory, plan, phases.append)
                self.assertTrue(result['active'])
                self.assertEqual(len(privileged), 1)
                self.assertEqual(privileged[0]['password'], session.password)
                record = Path(plan['profile']) / 'setup.json'
                self.assertEqual(json.loads(record.read_bytes())['phase'], 'started')
                for path in Path(plan['profile']).glob('*.json'):
                    self.assertNotIn(session.password.encode(), path.read_bytes())
                nas = json.loads(payloads[plan['base'] + '/helper.json'])
                self.assertEqual(nas['root'], '/share/Volume/Public/new-folder')
                self.assertEqual(nas['peers'], ['10.23.42.17'])
                self.assertEqual(nas['uid'], 1000)
                self.assertEqual(nas['clients'], {hashlib.sha256(original_run(['openssl', 'x509', '-in', str(Path(plan['profile']) / 'tls/client.pem'), '-outform', 'DER'])).hexdigest(): 'a' * 64})
                subprocess.run(['/bin/sh', '-n'], input=payloads[plan['base'] + '/service.sh'], check=True)
                self.assertEqual(len(setup.profiles()), 1)
                self.assertTrue(any(' start' in command for command in calls))


if __name__ == '__main__':
    unittest.main()
