import json
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile
import shlex
import shutil
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
            with self.assertRaisesRegex(setup.SetupError, 'already synchronized'):
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
                    share='Public', mountPoint='/mnt/ananas-' + 'a' * 16, port=18742, pin='b' * 64)
        system.validate(plan)
        for key, value in [('id', '../etc'), ('mountPoint', '/'), ('share', 'Public\npassword=x'), ('port', 22), ('pin', 'x')]:
            with self.subTest(key=key), self.assertRaises(ValueError):
                system.validate(dict(plan, **{key: value}))

    def test_system_mount_follows_a_moved_nas_by_its_pin(self):
        plan = dict(id='a' * 16, share='Public', mountPoint='/mnt/ananas-' + 'a' * 16, port=18742, pin='b' * 64, host='10.23.42.30',
                    route=dict(host='10.23.42.30', source='10.23.42.17', prefix='10.23.42.0/24', interface='enp1s0'))
        probed = []

        def probe(address):
            probed.append(address)
            return address == '10.23.42.77'
        self.assertEqual(system.find_nas(plan, [None, '10.23.42.30'], {'10.23.42.17'}, probe), '10.23.42.77')
        self.assertEqual(probed[0], '10.23.42.30')
        self.assertNotIn('10.23.42.17', probed)
        self.assertNotIn('10.23.42.0', probed)
        self.assertNotIn('10.23.42.255', probed)
        row = '36 25 0:50 / /mnt/ananas-' + 'a' * 16 + ' rw - cifs //{0}/Public rw,vers=3.1.1,seal\n'
        self.assertIsNone(system.mounted_host(plan, ''))
        self.assertEqual(system.mounted_host(plan, row.format('10.23.42.77')), '10.23.42.77')
        for bad in (row.format('10.23.43.77'), row.format('nas.local'), row.format('10.23.42.77').replace(',seal', '')):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                system.mounted_host(plan, bad)

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
                pc = json.loads((Path(plan['profile']) / 'config.json').read_bytes())
                for key in ('maxFileBytes', 'maxBatchBytes'):
                    self.assertEqual(nas[key], 8 << 30)
                    self.assertEqual(pc['sync'][key], nas[key])
                self.assertEqual(nas['root'], '/share/Volume/Public/new-folder')
                # DHCP may renumber the PC: authorize the direct subnet, not one lease.
                self.assertEqual(nas['peers'], ['10.23.42.0/24'])
                # The NAS may also be renumbered: it listens on its current address.
                self.assertEqual(nas['listen'], ':' + str(plan['port']))
                self.assertEqual(privileged[0]['pin'], pc['sync']['serverFingerprint'])
                self.assertNotIn('source', pc['sync'])
                service = payloads[plan['base'] + '/service.sh'].decode()
                self.assertIn('-s 10.23.42.0/24 -i', service)
                self.assertIn('-peer 10.23.42.0/24', service)
                self.assertNotIn('10.23.42.17', service)
                self.assertEqual(nas['uid'], 1000)
                self.assertEqual(nas['clients'], {hashlib.sha256(original_run(['openssl', 'x509', '-in', str(Path(plan['profile']) / 'tls/client.pem'), '-outform', 'DER'])).hexdigest(): 'a' * 64})
                subprocess.run(['/bin/sh', '-n'], input=payloads[plan['base'] + '/service.sh'], check=True)
                self.assertEqual(len(setup.profiles()), 1)
                self.assertTrue(any(' start' in command for command in calls))

    def test_managed_helper_matches_only_the_synchronized_root(self):
        inventory = dict(managed=[dict(root='/share/Volume/Public/Team', base='/share/Volume/.ananas-0123456789abcdef')])
        self.assertIsNotNone(setup.managed_helper(inventory, '/share/Volume/Public/Team/'))
        for path in ('/share/Volume/Public', '/share/Volume/Public/Team/Sub', '/share/Volume/Other'):
            self.assertIsNone(setup.managed_helper(inventory, path))

    def helper_fixture(self, directory, route, base='/share/Volume/.ananas-0123456789abcdef', legacy=False):
        """Trust material and configuration as an already-installed helper holds them."""
        pins = setup.certificates(directory, route['host'], '10.23.42.9')
        chain = 'AN0123456789ab'
        config = dict(liveWrites=True, root='/share/Volume/Public/Team', stateDir=base + '/state',
                      observerStateDir=base + '/observer', namespace='c' * 64, listen=route['host'] + ':18742',
                      interface='eth0', prefix=route['prefix'], peers=['10.23.42.9'], uid=1000, gid=100, groups=[100],
                      certificate=base + '/server.pem', privateKey=base + '/server.key', clientCA=base + '/ca.pem',
                      clients={pins['client']: 'a' * 64}, exclusions=['@Recycle/'], maxConnections=2,
                      maxFileBytes=8 << 30, maxBatchBytes=8 << 30, maxCacheBytes=1 << 40,
                      maxCacheEntries=1000000, readBytesPerSecond=1073741824)
        service = ('#!/bin/sh\nset -eu\ncase "${1:-}" in\nstart)\n if ! /sbin/iptables -nL %s >/dev/null 2>&1; then\n'
                   '  /sbin/iptables -N %s\n  /sbin/iptables -A %s -s 10.23.42.9 -i eth0 -j ACCEPT\n'
                   '  /sbin/iptables -A %s -j DROP\n  /sbin/iptables -I INPUT 1 -p tcp --dport 18742 -j %s\n fi\n ;;\n'
                   'stop) :;;\nstatus) :;;\n*) exit 2;;\nesac\n') % (chain, chain, chain, chain, chain)
        if legacy:
            # The historical live-trial script: separate inbound and outbound chains.
            service = ('#!/bin/sh\nset -eu\ncase "${1:-}" in\n  start)\n'
                       '    if ! /sbin/iptables -nL ANANAS_OUT >/dev/null 2>&1; then\n'
                       '      /sbin/iptables -N ANANAS_OUT\n'
                       '      /sbin/iptables -A ANANAS_OUT -d 10.23.42.9 -o eth0 -j ACCEPT\n'
                       '      /sbin/iptables -A ANANAS_OUT -j DROP\n'
                       '      /sbin/iptables -I OUTPUT 1 -p tcp --sport 8742 -j ANANAS_OUT\n    fi\n'
                       '    if ! /sbin/iptables -nL ANANAS_IN >/dev/null 2>&1; then\n'
                       '      /sbin/iptables -N ANANAS_IN\n'
                       '      /sbin/iptables -A ANANAS_IN -s 10.23.42.9 -i eth0 -j ACCEPT\n'
                       '      /sbin/iptables -A ANANAS_IN -j DROP\n'
                       '      /sbin/iptables -I INPUT 1 -p tcp --dport 8742 -j ANANAS_IN\n    fi\n    ;;\n'
                       '  stop) :;;\n  status) :;;\n  *) exit 2;;\nesac\n')
        return dict(config=config, service=service, chain=chain,
                    rules=setup.firewall_rules(service, config['peers']),
                    ca=(directory / 'ca.pem').read_bytes(), server=(directory / 'server.pem').read_bytes())

    def test_join_adds_this_pc_without_disturbing_the_installed_helper(self):
        """A second PC enrolls with its own issuer; no existing private key is reused."""
        route = dict(host='10.23.42.30', source='10.23.42.17', interface='enp1s0', prefix='10.23.42.0/24')
        base = '/share/Volume/.ananas-0123456789abcdef'
        share = dict(name='Public', path='/share/Volume/Public')
        managed = dict(name='anaNAS-0123456789abcdef', base=base, root='/share/Volume/Public/Team',
                       port=18742, peers=['10.23.42.9'])
        inventory = dict(shares=[share], accounts=[], managed=[managed], architecture='aarch64')
        payloads, calls = {}, []
        session = Mock(host=route['host'], route=route, username='admin', password='fake-join-secret')
        def remote(command, **kwargs):
            calls.append(command)
            tokens = shlex.split(command)
            if 'data' in kwargs:
                payloads[tokens[tokens.index('>') + 1].rstrip(';')] = kwargs['data']
            names = [t.rstrip(';') for t in tokens[tokens.index('cp') + 1:] if not t.startswith('-')] if 'cp' in tokens else []
            if len(names) == 2 and not command.startswith('command -v'):
                guard = tokens[tokens.index('-e') + 1].rstrip(';') if '-e' in tokens else None
                if not (guard == names[1] and names[1] in payloads):
                    payloads[names[1]] = payloads[names[0]]
            if 'mv' in tokens and not command.startswith('command -v'):
                source = tokens[tokens.index('mv') + 1]
                payloads[tokens[tokens.index('mv') + 2].rstrip(';')] = payloads.pop(source)
            if command.startswith('sha256sum '):
                return hashlib.sha256(payloads[tokens[1]]).hexdigest() + '  ' + tokens[1]
            if command.startswith('head -c'):
                return payloads[tokens[-1]].decode()
            if command.startswith('command -v '):
                return '/bin/' + tokens[-1]
            return ''
        session.command.side_effect = remote
        original_run = setup.run
        privileged = []
        def local(argv, **kwargs):
            if argv[0] == 'openssl':
                return original_run(argv, **kwargs)
            self.assertNotIn(session.password, repr(argv))
            if '-client-identity' in argv:
                return json.dumps(dict(clientID='b' * 64)).encode()
            if argv[0] == 'pkexec':
                privileged.append(json.loads(kwargs['data']))
            return b''
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            with patch.object(Path, 'home', return_value=home), patch.object(setup, 'config_home', return_value=home / '.config/nas-sync'), \
                    patch.object(setup, 'lan', return_value=route), patch.object(setup, 'run', side_effect=local), \
                    patch.object(setup.time, 'sleep'):
                existing = home / 'installed-helper'
                existing.mkdir()
                helper = self.helper_fixture(existing, route, base)
                payloads[base + '/helper.json'] = json.dumps(helper['config']).encode()
                payloads[base + '/service.sh'] = helper['service'].encode()
                payloads[base + '/ca.pem'] = helper['ca']
                payloads[base + '/server.pem'] = helper['server']
                read = setup.read_helper(session, managed)
                self.assertEqual([rule['chain'] for rule in read['rules']], [helper['chain']])
                self.assertEqual(read['config']['namespace'], 'c' * 64)
                plan = setup.make_join_plan(session, inventory, share, str(home / 'files'), managed, read)
                self.assertEqual((plan['port'], plan['namespace'], plan['uid']), (18742, 'c' * 64, os.getuid()))
                result = setup.join(session, inventory, plan, read, lambda message: None)
                self.assertIn(str(home / 'files'), result['message'])
                self.assertEqual(len(privileged), 1)
                nas = json.loads(payloads[base + '/helper.json'])
                pc = json.loads((Path(plan['profile']) / 'config.json').read_bytes())
                # The installed helper keeps its identity, root and first PC.
                self.assertEqual(nas['root'], helper['config']['root'])
                self.assertEqual(nas['namespace'], helper['config']['namespace'])
                self.assertEqual(nas['peers'], ['10.23.42.9', '10.23.42.17'])
                self.assertEqual(len(nas['clients']), 2)
                self.assertLessEqual(nas['maxConnections'], 8)
                self.assertGreaterEqual(nas['maxConnections'], 4)
                for pin, identity in helper['config']['clients'].items():
                    self.assertEqual(nas['clients'][pin], identity)
                # This PC speaks the helper's namespace with a replica identity of its own.
                self.assertEqual(pc['sync']['namespace'], nas['namespace'])
                self.assertNotEqual(pc['sync']['replicaNamespace'], pc['sync']['namespace'])
                self.assertEqual(pc['sync']['serverFingerprint'],
                                 hashlib.sha256(original_run(['openssl', 'x509', '-outform', 'DER'], data=helper['server'])).hexdigest())
                pin = hashlib.sha256(original_run(['openssl', 'x509', '-in', str(Path(plan['profile']) / 'tls/client.pem'), '-outform', 'DER'])).hexdigest()
                self.assertEqual(nas['clients'][pin], 'b' * 64)
                # The NAS trusts both issuers; the first PC's key never left its PC.
                self.assertEqual(payloads[base + '/ca.pem'].count(b'BEGIN CERTIFICATE'), 2)
                self.assertIn(helper['ca'].strip(), payloads[base + '/ca.pem'])
                self.assertNotIn(b'PRIVATE KEY', payloads[base + '/ca.pem'])
                updated = payloads[base + '/service.sh'].decode()
                subprocess.run(['/bin/sh', '-n'], input=payloads[base + '/service.sh'], check=True)
                self.assertLess(updated.index('-s 10.23.42.17 -i eth0 -j ACCEPT'), updated.index('-j DROP'))
                self.assertIn('-s 10.23.42.9 -i eth0 -j ACCEPT', updated)
                self.assertTrue(any('iptables -I ' + helper['chain'] + ' 1 -s 10.23.42.17' in c for c in calls))
                self.assertTrue(any('iptables -C ' + helper['chain'] + ' -s 10.23.42.17' in c for c in calls))
                self.assertTrue(any(' start' in c for c in calls))
                for path in Path(plan['profile']).glob('*.json'):
                    self.assertNotIn(session.password.encode(), path.read_bytes())
                self.assertEqual(json.loads((Path(plan['profile']) / 'setup.json').read_bytes())['phase'], 'started')

    def test_join_refuses_an_off_subnet_or_already_enrolled_pc(self):
        route = dict(host='10.23.42.30', source='10.23.42.17', interface='enp1s0', prefix='10.23.42.0/24')
        base = '/share/Volume/.ananas-0123456789abcdef'
        share = dict(name='Public', path='/share/Volume/Public')
        managed = dict(name='anaNAS-0123456789abcdef', base=base, root='/share/Volume/Public/Team', port=18742, peers=[])
        inventory = dict(shares=[share], accounts=[], managed=[managed])
        session = Mock(host=route['host'], route=route)
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            with patch.object(Path, 'home', return_value=home), patch.object(setup, 'config_home', return_value=home / '.config/nas-sync'):
                existing = home / 'installed-helper'
                existing.mkdir()
                helper = self.helper_fixture(existing, route, base)
                duplicate = dict(helper, config=dict(helper['config'], peers=['10.23.42.9', route['source']]))
                with self.assertRaisesRegex(setup.SetupError, 'already allowed'):
                    setup.make_join_plan(session, inventory, share, str(home / 'files'), managed, duplicate)
                elsewhere = dict(helper, config=dict(helper['config'], prefix='10.99.9.0/24', listen='10.99.9.30:18742'))
                with self.assertRaisesRegex(setup.SetupError, 'same directly connected LAN'):
                    setup.make_join_plan(session, inventory, share, str(home / 'files'), managed, elsewhere)
                outside = dict(managed, root='/share/Other/Team')
                with self.assertRaisesRegex(setup.SetupError, 'not inside the selected share'):
                    setup.make_join_plan(session, inventory, share, str(home / 'files'), outside, helper)

    def test_join_mirrors_every_chain_of_the_historical_live_trial_script(self):
        """The first deployment used separate inbound/outbound chains; both must allow the new PC."""
        route = dict(host='10.23.42.30', source='10.23.42.17', interface='enp1s0', prefix='10.23.42.0/24')
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            with patch.object(Path, 'home', return_value=home):
                existing = home / 'installed-helper'
                existing.mkdir()
                helper = self.helper_fixture(existing, route, legacy=True)
                rules = setup.firewall_rules(helper['service'], helper['config']['peers'])
                self.assertEqual(sorted(rule['chain'] for rule in rules), ['ANANAS_IN', 'ANANAS_OUT'])
                # A terminal DROP rule names no PC and must never be mirrored.
                self.assertTrue(all('DROP' not in rule['text'] for rule in rules))
                service, commands = helper['service'], []
                for rule in sorted(rules, key=lambda item: item['end'], reverse=True):
                    text = rule['text'].replace(rule['peer'], route['source'])
                    service = service[:rule['end']] + '\n' + rule['indent'] + text + service[rule['end']:]
                    commands.append(text)
                subprocess.run(['/bin/sh', '-n'], input=service.encode(), check=True)
                self.assertIn('/sbin/iptables -A ANANAS_IN -s 10.23.42.17 -i eth0 -j ACCEPT', commands)
                self.assertIn('/sbin/iptables -A ANANAS_OUT -d 10.23.42.17 -o eth0 -j ACCEPT', commands)
                for chain, direction in (('ANANAS_IN', '-s'), ('ANANAS_OUT', '-d')):
                    body = service[service.index('-nL ' + chain):]
                    self.assertLess(body.index(direction + ' 10.23.42.17'), body.index('-j DROP'))
                    self.assertLess(body.index(direction + ' 10.23.42.9'), body.index('-j DROP'))

    def test_firewall_rules_ignore_unrelated_addresses_and_chains(self):
        service = ('#!/bin/sh\n'
                   ' /sbin/iptables -A ANANAS_IN -s 10.23.42.9 -i eth0 -j ACCEPT\n'
                   ' /sbin/iptables -A ANANAS_IN -s 10.23.42.99 -i eth0 -j ACCEPT\n'
                   ' /sbin/iptables -A OTHER -s 10.23.42.50 -i eth0 -j ACCEPT\n'
                   ' /sbin/iptables -A ANANAS_IN -j DROP\n')
        rules = setup.firewall_rules(service, ['10.23.42.9'])
        self.assertEqual([rule['text'] for rule in rules],
                         ['/sbin/iptables -A ANANAS_IN -s 10.23.42.9 -i eth0 -j ACCEPT'])
        self.assertEqual(setup.firewall_rules(service, []), [])

    def join_environment(self, home, route, fail=None):
        """A simulated NAS holding an installed helper, with optional failing commands."""
        base = '/share/Volume/.ananas-0123456789abcdef'
        payloads, calls, state = {}, [], {'fail': fail}
        session = Mock(host=route['host'], route=route, username='admin', password='fake-join-secret')
        def remote(command, **kwargs):
            calls.append(command)
            tokens = shlex.split(command)
            if state['fail'] and state['fail'](command):
                raise setup.SetupError('The QNAP connection ended before this step completed.')
            if 'data' in kwargs:
                payloads[tokens[tokens.index('>') + 1].rstrip(';')] = kwargs['data']
            names = [t.rstrip(';') for t in tokens[tokens.index('cp') + 1:] if not t.startswith('-')] if 'cp' in tokens else []
            if len(names) == 2 and not command.startswith('command -v'):
                guard = tokens[tokens.index('-e') + 1].rstrip(';') if '-e' in tokens else None
                if not (guard == names[1] and names[1] in payloads):
                    payloads[names[1]] = payloads[names[0]]
            if 'mv' in tokens and not command.startswith('command -v'):
                payloads[tokens[tokens.index('mv') + 2].rstrip(';')] = payloads.pop(tokens[tokens.index('mv') + 1])
            if command.startswith('sha256sum '):
                return hashlib.sha256(payloads[tokens[1]]).hexdigest() + '  ' + tokens[1]
            if command.startswith('head -c'):
                return payloads[tokens[-1]].decode()
            if command.startswith('command -v '):
                return '/bin/' + tokens[-1]
            if command.startswith('test -e ') and 'echo yes' in command:
                return 'yes\n' if tokens[2] in payloads else ''
            return ''
        session.command.side_effect = remote
        existing = home / 'installed-helper'
        existing.mkdir()
        helper = self.helper_fixture(existing, route, base)
        payloads[base + '/helper.json'] = json.dumps(helper['config']).encode()
        payloads[base + '/service.sh'] = helper['service'].encode()
        payloads[base + '/ca.pem'] = helper['ca']
        payloads[base + '/server.pem'] = helper['server']
        managed = dict(name='anaNAS-0123456789abcdef', base=base, root='/share/Volume/Public/Team',
                       port=18742, peers=['10.23.42.9'])
        inventory = dict(shares=[dict(name='Public', path='/share/Volume/Public')], accounts=[], managed=[managed])
        return dict(session=session, payloads=payloads, calls=calls, helper=helper, managed=managed,
                    inventory=inventory, base=base, share=inventory['shares'][0], state=state)

    def run_join(self, home, environment, folder='files'):
        original_run = setup.run
        def local(argv, **kwargs):
            if argv[0] == 'openssl':
                return original_run(argv, **kwargs)
            if '-client-identity' in argv:
                return json.dumps(dict(clientID='b' * 64)).encode()
            return b''
        with patch.object(setup, 'run', side_effect=local):
            read = setup.read_helper(environment['session'], environment['managed'])
            plan = setup.make_join_plan(environment['session'], environment['inventory'],
                                        environment['share'], str(home / folder), environment['managed'], read)
            return plan, setup.join(environment['session'], environment['inventory'], plan, read, lambda message: None)

    def test_join_proceeds_when_the_helper_cannot_verify_a_configuration_in_advance(self):
        """Helpers predating -check-config still enrol; the restart proves the result."""
        route = dict(host='10.23.42.30', source='10.23.42.17', interface='enp1s0', prefix='10.23.42.0/24')
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            with patch.object(Path, 'home', return_value=home), patch.object(setup, 'config_home', return_value=home / '.config/nas-sync'), \
                    patch.object(setup, 'lan', return_value=route), patch.object(setup.time, 'sleep'):
                environment = self.join_environment(home, route, fail=lambda c: '-check-config' in c)
                plan, result = self.run_join(home, environment)
                self.assertIn(str(home / 'files'), result['message'])
                nas = json.loads(environment['payloads'][environment['base'] + '/helper.json'])
                self.assertEqual(nas['peers'], ['10.23.42.9', '10.23.42.17'])
                # Probed once before any change, and never required afterwards.
                self.assertEqual(len([c for c in environment['calls'] if '-check-config' in c]), 1)
                self.assertTrue(any(c.endswith(' status') for c in environment['calls']))

    def test_join_restores_the_installed_helper_when_it_does_not_restart(self):
        """A failed restart must leave the already-enrolled PC's helper as it was."""
        route = dict(host='10.23.42.30', source='10.23.42.17', interface='enp1s0', prefix='10.23.42.0/24')
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            with patch.object(Path, 'home', return_value=home), patch.object(setup, 'config_home', return_value=home / '.config/nas-sync'), \
                    patch.object(setup, 'lan', return_value=route), patch.object(setup.time, 'sleep'):
                starts = []
                def fail(command):
                    if command.endswith(' start'):
                        starts.append(command)
                        return len(starts) == 1
                    return False
                environment = self.join_environment(home, route, fail=fail)
                original = dict(environment['payloads'])
                with self.assertRaisesRegex(setup.SetupError, 'previous configuration was restored'):
                    self.run_join(home, environment)
                base = environment['base']
                for name in ('helper.json', 'ca.pem', 'service.sh'):
                    self.assertEqual(environment['payloads'][base + '/' + name], original[base + '/' + name])
                self.assertEqual(json.loads(environment['payloads'][base + '/helper.json'])['peers'], ['10.23.42.9'])
                self.assertTrue(any(c.endswith(' status') for c in environment['calls']))

    def test_a_staged_attempt_is_replaced_so_a_retry_can_proceed(self):
        route = dict(host='10.23.42.30', source='10.23.42.17', interface='enp1s0', prefix='10.23.42.0/24')
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            with patch.object(Path, 'home', return_value=home), patch.object(setup, 'config_home', return_value=home / '.config/nas-sync'), \
                    patch.object(setup, 'lan', return_value=route), patch.object(setup.time, 'sleep'):
                environment = self.join_environment(home, route, fail=lambda c: c.startswith('sha256sum'))
                with self.assertRaises(setup.SetupError):
                    self.run_join(home, environment)
                profile = next((home / '.config/nas-sync/profiles').iterdir())
                self.assertEqual(json.loads((profile / 'setup.json').read_bytes())['phase'], 'nas-updating')
                self.assertFalse(setup.staged_attempt(profile))
                # A record past staging is for a person to read, not to overwrite.
                with self.assertRaisesRegex(setup.SetupError, 'reached the NAS'):
                    self.run_join(home, environment, folder='other')
                setup.atomically(profile / 'setup.json', json.dumps({'phase': 'staging', 'plan': {}}).encode())
                self.assertTrue(setup.staged_attempt(profile))

    def test_an_interrupted_attempt_is_repaired_from_the_saved_originals(self):
        """An attempt that wrote helper.json and then failed must be cleanly redoable."""
        route = dict(host='10.23.42.30', source='10.23.42.17', interface='enp1s0', prefix='10.23.42.0/24')
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            with patch.object(Path, 'home', return_value=home), patch.object(setup, 'config_home', return_value=home / '.config/nas-sync'), \
                    patch.object(setup, 'lan', return_value=route), patch.object(setup.time, 'sleep'):
                # Stop just after helper.json is written, as an interrupted attempt did.
                environment = self.join_environment(home, route, fail=lambda c: c.endswith(' stop || true'))
                base = environment['base']
                with self.assertRaises(setup.SetupError):
                    self.run_join(home, environment)
                interrupted = json.loads(environment['payloads'][base + '/helper.json'])
                self.assertEqual(interrupted['peers'], ['10.23.42.9', '10.23.42.17'])
                self.assertIn(base + '/helper.json.before-enroll', environment['payloads'])
                stale = set(interrupted['clients']) - set(environment['helper']['config']['clients'])
                self.assertEqual(len(stale), 1)
                shutil.rmtree(next((home / '.config/nas-sync/profiles').iterdir()))
                environment['state']['fail'] = None
                plan, result = self.run_join(home, environment, folder='again')
                self.assertTrue(plan['repair'])
                nas = json.loads(environment['payloads'][base + '/helper.json'])
                # Rebuilt from the saved original: the enrolled PC and exactly one new PC.
                self.assertEqual(nas['peers'], ['10.23.42.9', '10.23.42.17'])
                self.assertEqual(len(nas['clients']), 2)
                self.assertFalse(stale & set(nas['clients']), "the failed attempt's certificate must not persist")
                self.assertEqual(environment['payloads'][base + '/ca.pem'].count(b'BEGIN CERTIFICATE'), 2)
                self.assertIn(str(home / 'again'), result['message'])

    def test_only_an_unfinished_additional_setup_can_be_discarded(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            with patch.object(Path, 'home', return_value=home), patch.object(setup, 'config_home', return_value=home / '.config/nas-sync'):
                profile = home / '.config/nas-sync/profiles/0123456789abcdef'
                profile.mkdir(parents=True)
                setup.private_write(profile / 'config.json', b'{}')
                for phase, unfinished in (('staging', True), ('nas-updating', True), ('started', False)):
                    setup.atomically(profile / 'setup.json', json.dumps({'phase': phase}).encode())
                    self.assertEqual(setup.incomplete_attempt(profile), unfinished)
                # A finished setup is never discarded by this path.
                with self.assertRaisesRegex(setup.SetupError, 'unfinished setup'):
                    setup.discard_attempt(profile)
                self.assertTrue(profile.exists())
                # Nor is the primary profile, which lives outside profiles/.
                primary = home / '.config/nas-sync'
                setup.atomically(primary / 'setup.json', json.dumps({'phase': 'staging'}).encode())
                with self.assertRaisesRegex(setup.SetupError, 'unfinished setup'):
                    setup.discard_attempt(primary)
                self.assertTrue((primary / 'setup.json').exists())
                setup.atomically(profile / 'setup.json', json.dumps({'phase': 'nas-updating'}).encode())
                with patch.object(setup, 'run'):
                    setup.discard_attempt(profile)
                self.assertFalse(profile.exists())
                # A profile with no record at all predates recovery records and stands.
                self.assertFalse(setup.incomplete_attempt(home / 'absent'))

    def test_only_unclaimed_synchronization_state_is_released(self):
        """An index is bound to its local folder, so abandoned state must go; state a
        configured profile still uses must not."""
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            with patch.object(Path, 'home', return_value=home), patch.object(setup, 'config_home', return_value=home / '.config/nas-sync'):
                for identifier in ('0123456789abcdef', 'fedcba9876543210'):
                    state = setup.state_home(identifier)
                    state.mkdir(parents=True)
                    setup.private_write(state / 'index.db', b'fixture')
                profile = home / '.config/nas-sync/profiles/fedcba9876543210'
                profile.mkdir(parents=True)
                setup.private_write(profile / 'config.json', json.dumps(
                    {'local': {'root': str(home / 'files')}, 'stateDir': str(setup.state_home('fedcba9876543210'))}).encode())
                setup.release_state('fedcba9876543210')
                self.assertTrue(setup.state_home('fedcba9876543210').exists())
                setup.release_state('0123456789abcdef')
                self.assertFalse(setup.state_home('0123456789abcdef').exists())
                setup.release_state('never-created')

    def test_an_unfinished_attempt_does_not_reserve_its_local_folder(self):
        """A failed setup synchronizes nothing and must not block that folder forever."""
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            with patch.object(Path, 'home', return_value=home), patch.object(setup, 'config_home', return_value=home / '.config/nas-sync'):
                profile = home / '.config/nas-sync/profiles/0123456789abcdef'
                profile.mkdir(parents=True)
                setup.private_write(profile / 'config.json', json.dumps({'local': {'root': str(home / 'files')}}).encode())
                setup.atomically(profile / 'setup.json', json.dumps({'phase': 'staging'}).encode())
                self.assertEqual(setup.validate_local(str(home / 'files')), home / 'files')
                # A finished setup still owns its folder, and its parents and children.
                setup.atomically(profile / 'setup.json', json.dumps({'phase': 'started'}).encode())
                for candidate in (home / 'files', home / 'files/inside', home):
                    with self.assertRaisesRegex(setup.SetupError, 'overlaps an existing sync folder|entire home'):
                        setup.validate_local(str(candidate))


if __name__ == '__main__':
    unittest.main()
