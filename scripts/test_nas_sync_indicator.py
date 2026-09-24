import unittest
from unittest.mock import Mock, patch
from pathlib import Path

from nas_sync_indicator import Client, Indicator, TokenParser, argb, icon_directory, icon_pixmaps, interface_xml, user_error
from gi.repository import Gio, GLib


class IndicatorTests(unittest.TestCase):
    def test_icon_is_found_where_the_installer_puts_it(self):
        import tempfile
        with tempfile.TemporaryDirectory() as data, patch.dict('os.environ', {'XDG_DATA_HOME': data}):
            self.assertEqual(icon_directory(), Path(data) / 'anaNAS/icons')
            hicolor = Path(data) / 'icons/hicolor/scalable/apps'
            hicolor.mkdir(parents=True)
            (hicolor / 'ananas-symbolic.svg').write_text('<svg/>')
            self.assertEqual(icon_directory(), hicolor)
            private = Path(data) / 'anaNAS/icons'
            private.mkdir(parents=True)
            (private / 'ananas-symbolic.svg').write_text('<svg/>')
            self.assertEqual(icon_directory(), private)

    def test_pixmap_fallback_is_argb_and_light(self):
        self.assertEqual(argb(1, 1, 4, bytes((1, 2, 3, 4))), bytes((4, 1, 2, 3)))
        pixmaps = icon_pixmaps(Path(__file__).resolve().parent.parent / 'internal/web/assets/ananas-symbolic.svg', (22,))
        self.assertEqual(len(pixmaps), 1)
        width, height, data = pixmaps[0]
        self.assertEqual((width, height, len(data)), (22, 22, 22 * 22 * 4))
        drawn = [data[i:i + 4] for i in range(0, len(data), 4) if data[i] > 128]
        self.assertTrue(drawn)
        self.assertTrue(all(pixel[1] > 128 for pixel in drawn), 'strokes must be light for the dark top bar')
        self.assertEqual(icon_pixmaps(Path('/nonexistent.svg')), [])

    def test_long_errors_and_paths_do_not_expand_menu_labels(self):
        with patch('nas_sync_indicator.Gio.bus_own_name'):
            indicator = Indicator(Client(8721), Path('/tmp/local'))
        indicator.bus = Mock()
        indicator.error = None
        reason = 'Eligible files synced; some paths need attention: ' + 'long folder/' * 100
        indicator.state = dict(paused=False, automaticWrites=True, syncReason=reason,
                               recentUpdates=[dict(path='long folder/' * 100 + '\nfile', direction='to NAS')])
        indicator.changed()
        self.assertEqual(indicator.summary(), 'anaNAS — Synced eligible files · some paths need attention')
        self.assertIn('Pending files', indicator.items()[3][0])
        self.assertEqual(indicator.state['syncReason'], reason)
        for key, (label, _) in indicator.items().items():
            self.assertLessEqual(len(label), 72)
            self.assertNotIn('\n', label)
            self.assertLessEqual(len(indicator.properties(key, [])['label'].unpack()), 72)
        self.assertTrue(indicator.items()[9][0].endswith('…'))
        indicator.action_error = 'Failed\n' + 'x' * 2000
        indicator.changed()
        self.assertLessEqual(len(indicator.items()[3][0]), 72)
        indicator.state['syncReason'] = 'Unexpected error ' * 100
        self.assertLessEqual(len(indicator.summary()), 72)

    def test_unconfigured_installation_offers_setup_not_a_crash_message(self):
        with patch('nas_sync_indicator.Gio.bus_own_name'):
            indicator = Indicator(Client(8721), Path('/tmp/not-created'), configured=False)
        self.assertEqual(indicator.summary(), 'anaNAS — setup required')
        self.assertTrue(indicator.items()[21][1])
        self.assertFalse(indicator.items()[5][1])
        self.assertFalse(indicator.items()[7][1])
        with patch.object(Gio.Subprocess, 'new') as launch:
            indicator.event(6, 'clicked')
            self.assertIn('--setup', launch.call_args.args[0])

    def test_recent_updates_are_only_children_of_submenu(self):
        with patch('nas_sync_indicator.Gio.bus_own_name'):
            indicator = Indicator(Client(8721), Path('/tmp/local'))
        parent, props, children = indicator.layout(0, -1, [])
        rows = [row.unpack() for row in children]
        self.assertEqual([row[0] for row in rows], [1, 2, 3, 8, 4, 5, 6, 21, 7])
        submenu = next(row for row in rows if row[0] == 8)
        self.assertEqual(submenu[1]['children-display'], 'submenu')
        self.assertEqual([row[0] for row in submenu[2]], list(range(9, 21)))
        self.assertEqual(indicator.layout(8, 0, [])[2], [])
        # Exact return signature consumed by GNOME, including nested variants.
        GLib.Variant('(u(ia{sv}av))', (0, indicator.layout(0, -1, [])))
    def test_token_parser_rejects_missing_and_non_hex_tokens(self):
        for value in ("bad", "g" * 64, "0" * 65):
            parser = TokenParser()
            parser.feed(f'<meta name="nas-sync-csrf" content="{value}">')
            self.assertIsNone(parser.token)
        parser = TokenParser()
        parser.feed('<meta content="' + 'a' * 64 + '" name="nas-sync-csrf">')
        self.assertEqual(parser.token, 'a' * 64)

    def test_only_literal_loopback_without_proxy_or_redirect_client(self):
        for port in (True, 0, -1, 65536, "8721"):
            with self.assertRaises(ValueError):
                Client(port)
        with patch.dict("os.environ", {"http_proxy": "http://untrusted.example:8080"}):
            connection = Client(8721).connection()
            self.assertEqual(connection.host, "127.0.0.1")
            self.assertEqual(connection.port, 8721)
            self.assertEqual(connection.timeout, 5)

    def test_bus_contract_parses(self):
        interfaces = Gio.DBusNodeInfo.new_for_xml(interface_xml()).interfaces
        self.assertEqual([i.name for i in interfaces], ["org.kde.StatusNotifierItem", "com.canonical.dbusmenu"])

    def test_metadata_churn_does_not_redraw_the_desktop_panel(self):
        # Exercise the real panel presentation while replacing only the bus.
        with patch('nas_sync_indicator.Gio.bus_own_name'):
            indicator = Indicator(Client(8721), Path('/tmp/local'))
        indicator.bus = Mock()
        indicator.error = None
        indicator.state = {'paused': False, 'automaticWrites': False, 'uploadBytes': 0, 'downloadBytes': 0}
        indicator.changed()
        indicator.bus.reset_mock()
        for count in range(1000):
            indicator.state['events'] = count
            indicator.state['metadataReads'] = count
            indicator.changed()
        indicator.bus.emit_signal.assert_not_called()

        indicator.state['uploadBytes'] = 65536
        indicator.changed()
        self.assertEqual([call.args[3] for call in indicator.bus.emit_signal.call_args_list], ['ItemsPropertiesUpdated'])
        changed, removed = indicator.bus.emit_signal.call_args.args[4].unpack()
        self.assertEqual([item_id for item_id, _ in changed], [2])
        self.assertEqual(removed, [])
        indicator.bus.reset_mock()
        indicator.state['paused'] = True
        indicator.changed()
        self.assertEqual([call.args[3] for call in indicator.bus.emit_signal.call_args_list], ['ItemsPropertiesUpdated', 'NewOverlayIcon', 'NewTitle'])

    def test_first_daemon_status_does_not_cancel_initial_menu_layout(self):
        with patch('nas_sync_indicator.Gio.bus_own_name'):
            indicator = Indicator(Client(8721), Path('/tmp/local'))
        indicator.bus = Mock()
        indicator.error = None
        indicator.state = {'paused': False, 'automaticWrites': True,
                           'syncReason': 'LAN sync active',
                           'uploadBytes': 1, 'downloadBytes': 2}

        indicator.changed()

        signals = [call.args[3] for call in indicator.bus.emit_signal.call_args_list]
        self.assertNotIn('LayoutUpdated', signals)
        self.assertIn('ItemsPropertiesUpdated', signals)
        self.assertEqual(indicator.properties(1, [])['label'].unpack(),
                         'anaNAS — LAN sync active')

    def test_action_errors_are_contextual_and_notified(self):
        with patch('nas_sync_indicator.Gio.bus_own_name'):
            indicator = Indicator(Client(8721), Path('/tmp/local'))
        indicator.bus = Mock()
        indicator.error = None
        indicator.state = {'paused': False, 'automaticWrites': True,
                           'syncReason': 'LAN sync active'}
        indicator.busy = True
        indicator.control_done(user_error('Could not refresh anaNAS status',
                                          ConnectionRefusedError('connection refused')))

        self.assertEqual(indicator.action_error,
                         'Could not refresh anaNAS status: connection refused')
        self.assertIsNone(indicator.error)
        self.assertEqual(indicator.summary(), 'anaNAS — LAN sync active')
        self.assertTrue(indicator.items()[4][1])
        self.assertEqual(indicator.properties(3, [])['label'].unpack(),
                         'Problem: Could not refresh anaNAS status: connection refused')
        indicator.bus.call.assert_called_once()

    def test_failed_folder_open_preserves_stream_status_and_controls(self):
        with patch('nas_sync_indicator.Gio.bus_own_name'):
            indicator = Indicator(Client(8721), Path('/tmp/local'))
        indicator.bus = Mock()
        indicator.error = None
        indicator.state = {'paused': False, 'automaticWrites': True,
                           'syncReason': 'LAN sync active', 'uploadBytes': 42}
        with patch.object(Gio.AppInfo, 'launch_default_for_uri_finish',
                          side_effect=GLib.Error('launch failed')):
            indicator.open_done(None, None, 'Could not open NASdir', True)
        self.assertEqual(indicator.summary(), 'anaNAS — LAN sync active')
        self.assertEqual(indicator.overlay(), '')
        self.assertTrue(indicator.items()[4][1])
        self.assertIn('42.0 B', indicator.items()[2][0])
        self.assertIn('file manager could not open', indicator.items()[3][0])
        self.assertNotIn('pygi-error', indicator.items()[3][0])
        self.assertNotIn('launch failed', indicator.items()[3][0])
        self.assertIn('/tmp/local', indicator.action_error)
        self.assertLessEqual(len(indicator.items()[3][0]), 72)
        notification = indicator.bus.call.call_args.args[4].unpack()[4]
        self.assertEqual(notification, indicator.action_error)
        # A subsequent stream delivery must not erase the action error.
        indicator.pending = (indicator.state, None)
        indicator.apply_delivery()
        self.assertIn('file manager could not open', indicator.items()[3][0])
        with patch.object(Gio.AppInfo, 'launch_default_for_uri_finish'):
            indicator.open_done(None, None, 'Could not open NASdir')
        self.assertIsNone(indicator.action_error)
        indicator.error = 'connection refused'
        indicator.action_done(None)
        self.assertEqual(indicator.summary(), 'anaNAS — daemon unavailable')
        self.assertFalse(indicator.items()[4][1])

    def test_nautilus_disconnected_launch_retries_in_separate_unit(self):
        with patch('nas_sync_indicator.Gio.bus_own_name'):
            indicator = Indicator(Client(8721), Path('/tmp/local folder'))
        error = GLib.Error.new_literal(Gio.dbus_error_quark(),
                                      'recipient disconnected', Gio.DBusError.NO_REPLY)
        app = Mock()
        app.get_id.return_value = 'org.gnome.Nautilus.desktop'
        with patch.object(Gio.AppInfo, 'launch_default_for_uri_finish', side_effect=error), \
                patch.object(Gio.AppInfo, 'get_default_for_type', return_value=app), \
                patch.object(Gio.Subprocess, 'new') as launch:
            indicator.open_done(None, None, 'Could not open NASdir', True)
            argv = launch.call_args.args[0]
            self.assertEqual(argv[:2], ['/usr/bin/systemd-run', '--user'])
            self.assertIn('GSK_RENDERER=cairo', argv)
            self.assertEqual(argv[-1], 'file:///tmp/local%20folder')
            process = launch.return_value
            process.wait_check_async.assert_called_once()
            process.wait_check_async.call_args.args[1](process, None)
            self.assertIsNone(indicator.action_error)
            process.wait_check_finish.side_effect = GLib.Error('fallback failed')
            indicator.fallback_done(process, None, 'Could not open NASdir')
            self.assertIn('file manager could not open', indicator.action_error)
            self.assertNotIn('fallback failed', indicator.action_error)
            launch.reset_mock()
            app.get_id.return_value = 'another-file-manager.desktop'
            indicator.open_done(None, None, 'Could not open NASdir', True)
            launch.assert_not_called()
            indicator.open_done(None, None, 'Could not open status', False)
            launch.assert_not_called()

    def test_open_errors_explain_failure_without_raw_exceptions(self):
        with patch('nas_sync_indicator.Gio.bus_own_name'):
            indicator = Indicator(Client(8721), Path('/tmp/local'))
        for domain, code, explanation in (
            (Gio.dbus_error_quark(), Gio.DBusError.NO_REPLY, 'file manager stopped responding'),
            (Gio.io_error_quark(), Gio.IOErrorEnum.PERMISSION_DENIED, 'Access to this location was denied'),
            (Gio.io_error_quark(), Gio.IOErrorEnum.NOT_FOUND, 'could not be found'),
        ):
            with self.subTest(code=code):
                error = GLib.Error.new_literal(domain, 'technical detail for logs only', code)
                indicator.report_open_error('Could not open NASdir', error, True)
                self.assertIn(explanation, indicator.action_error)
                self.assertIn('Try again', indicator.action_error)
                self.assertNotIn('technical detail', indicator.action_error)
        with patch.object(Gio.AppInfo, 'launch_default_for_uri_async',
                          side_effect=GLib.Error('synchronous launch failure')):
            indicator.event(5, 'clicked')
        self.assertIn('file manager could not open', indicator.action_error)

    def test_cached_menu_labels_follow_disabled_active_pause_and_resume(self):
        with patch('nas_sync_indicator.Gio.bus_own_name'):
            indicator = Indicator(Client(8721), Path('/tmp/local'))
        indicator.bus = Mock()
        indicator.error = None
        indicator.state = {'paused': False, 'automaticWrites': False, 'syncReason': 'disabled'}
        indicator.changed()
        # Like GNOME, cache existing item properties after initial layout.
        cached = {i: {key: value.unpack() for key, value in indicator.properties(i, []).items()}
                  for i in indicator.items()}
        for paused, title in ((False, 'anaNAS — LAN sync active'), (True, 'anaNAS — paused'), (False, 'anaNAS — LAN sync active')):
            indicator.bus.reset_mock()
            indicator.state.update(automaticWrites=True, paused=paused, syncReason='LAN sync active')
            indicator.changed()
            signals = indicator.bus.emit_signal.call_args_list
            self.assertNotIn('LayoutUpdated', [call.args[3] for call in signals])
            for call in signals:
                if call.args[3] == 'ItemsPropertiesUpdated':
                    changed, removed = call.args[4].unpack()
                    self.assertEqual(removed, [])
                    for item_id, values in changed:
                        cached[item_id].update(values)
            self.assertEqual(cached[1]['label'], title)
            self.assertEqual(cached[4]['label'], 'Resume sync' if paused else 'Pause sync')

    def test_recent_updates_have_fixed_slots_and_24_hour_count(self):
        with patch('nas_sync_indicator.Gio.bus_own_name'):
            indicator = Indicator(Client(8721), Path('/tmp/local'))
        indicator.error = None
        indicator.state = {
            'paused': False, 'automaticWrites': True, 'syncReason': 'LAN sync active',
            'updatedLast24Hours': 9828,
            'recentUpdates': [
                {'path': 'SampleDocuments/paper.pdf', 'direction': 'to NAS',
                 'action': 'added', 'at': '2026-09-10T12:34:00Z'},
                {'path': 'notes.txt', 'direction': 'from NAS',
                 'action': 'updated', 'at': '2026-09-10T11:00:00Z'},
            ],
        }
        self.assertEqual(indicator.items()[8][0],
                         'Recent updates — 9828 in the last 24 hours')
        self.assertIn('↑ SampleDocuments/paper.pdf (added)', indicator.items()[9][0])
        self.assertIn('↓ notes.txt (updated)', indicator.items()[10][0])
        self.assertTrue(indicator.item_visible(9))
        self.assertTrue(indicator.item_visible(10))
        self.assertFalse(indicator.item_visible(11))


if __name__ == "__main__":
    unittest.main()
