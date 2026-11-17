"""Native widget regression checks; run with an available desktop display."""
import unittest
from pathlib import Path
from unittest.mock import Mock, patch
from ananas_control_panel import Panel, Gtk, GLib, Gio, size, local_time


class ProgressTests(unittest.TestCase):
    def test_go_nanosecond_timestamps_and_missing_times(self):
        self.assertEqual(local_time('2026-11-10T16:09:47.044721251Z'),
                         local_time('2026-11-10T16:09:47Z'))
        self.assertNotEqual(local_time('2026-11-10T16:09:47.044721251Z'), '—')
        for value in (None, '', 'invalid'):
            self.assertEqual(local_time(value), '—')

    def test_current_file_percentage_and_pending_count(self):
        item = dict(name='folder', path='folder', localBytes=1, syncedBytes=999, files=2, syncedFiles=1)
        self.assertEqual(Panel.row(None, item, 1)[2], '50.0% · 1 pending')
        self.assertEqual(Panel.row(None, dict(item, files=10000, syncedFiles=9999), 1)[2], '99.9% · 1 pending')
        self.assertEqual(Panel.row(None, dict(item, syncedFiles=2), 1)[2], '100.0% · 0 pending')
        self.assertEqual(Panel.row(None, dict(item, files=0, syncedFiles=0), 1)[2], 'No files')
        del item['syncedFiles']
        self.assertEqual(Panel.row(None, item, 1)[2], 'Unknown')


class PanelTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if not Gtk.init_check()[0]:
            raise unittest.SkipTest("GTK display unavailable")
        cls.panel = Panel(Mock(), Path('/tmp/NASdir'))
        cls.panel.set_application_id('org.ananas.ControlPanel.WidgetTest')
        cls.panel.set_flags(Gio.ApplicationFlags.NON_UNIQUE)
        cls.panel.register(None)

    def test_pending_tab_confirms_selection_all_and_handles_stale_errors(self):
        client = Mock()
        panel = self.panel
        panel.client = client
        panel.build()
        panel.connected = True
        panel.submit = Mock(return_value=True)
        panel.load_pending('')
        self.assertFalse(panel.confirm_all.get_sensitive())
        work, done = panel.submit.call_args.args
        work()
        client.pending_files.assert_called_once_with('')
        item = dict(path='folder/file', size=3 << 30, action='Upload', reason='Waiting to sync', confirmable=True, generation=7)
        page = dict(items=[item], next='folder/file', total=250)
        done(page, None)
        self.assertTrue(panel.confirm_all.get_sensitive())
        self.assertTrue(panel.pending_more.get_sensitive())
        self.assertFalse(panel.confirm_one.get_sensitive())
        panel.pending_view.get_selection().select_path(Gtk.TreePath.new_from_string('0'))
        self.assertTrue(panel.confirm_one.get_sensitive())
        panel.confirm_pending(False)
        self.assertFalse(panel.confirm_all.get_sensitive())
        work, done = panel.submit.call_args.args
        work()
        client.confirm_sync.assert_called_once_with('folder/file', 7, False)
        done(dict(queued=1, blocked=0), None)
        # Confirmation must not optimistically remove a still-pending file.
        self.assertEqual(len(panel.pending_model), 1)
        panel.submit.call_args.args[1](page, None)
        panel.confirm_pending(True)
        work, done = panel.submit.call_args.args
        work()
        client.confirm_sync.assert_called_with(None, None, True)
        done(None, 'stale request')
        self.assertIn('refresh', panel.pending_result.get_text())
        panel.load_pending('')
        panel.submit.call_args.args[1](None, 'offline')
        self.assertFalse(panel.confirm_all.get_sensitive())
        self.assertFalse(panel.confirm_one.get_sensitive())
        panel.window.destroy()

    def test_pending_file_location_actions_include_blocked_and_deleted_entries(self):
        panel = self.panel
        panel.build()
        panel.submit = Mock(return_value=True)
        panel.window.show_all()
        panel.notebook.set_current_page(panel.notebook.page_num(panel.pending_page))
        panel.submit.call_args.args[1](dict(items=[], next='', total=0), None)
        while Gtk.events_pending():
            Gtk.main_iteration_do(False)
        panel.pending_model.append(['folder with spaces/$HOME/\\', '0 B', 'Upload', 'Unsupported name', False, '1'])
        panel.pending_model.append(['removed-file', '0 B', 'Delete from NAS', 'Waiting', True, '2'])
        panel.pending_model.append(['../outside/file', '0 B', 'Upload', 'Invalid', False, '3'])
        path = Gtk.TreePath.new_from_string('0')
        with patch.object(panel, 'open_directory') as directory, \
                patch.object(panel, 'open_terminal') as terminal, \
                patch.object(Gtk.Menu, 'popup_at_rect'), \
                patch.object(Gtk.Menu, 'popup_at_pointer'):
            panel.pending_view.emit('row-activated', path, panel.pending_view.get_column(0))
            directory.assert_called_once_with('folder with spaces/$HOME')
            directory.reset_mock()
            panel.pending_view.get_selection().select_path(path)
            self.assertTrue(panel.pending_keyboard_menu(panel.pending_view))
            actions = panel.pending_menu.get_children()
            self.assertEqual([a.get_label() for a in actions], ['Open containing folder', 'Open terminal here', 'Delete…'])
            actions[0].activate()
            actions[1].activate()
            directory.assert_called_once_with('folder with spaces/$HOME')
            terminal.assert_called_once_with('folder with spaces/$HOME')
            tree = Mock()
            tree.get_path_at_pos.return_value = (path, None, 0, 0)
            self.assertTrue(panel.pending_button_press(tree, Mock(button=3, x=1, y=2)))
            tree.set_cursor.assert_called_once_with(path)
            tree.get_path_at_pos.return_value = None
            self.assertFalse(panel.pending_button_press(tree, Mock(button=3, x=1, y=2)))
            panel.activate_pending(None, Gtk.TreePath.new_from_string('1'), None)
            directory.assert_called_with('.')
            directory.reset_mock()
            panel.activate_pending(None, Gtk.TreePath.new_from_string('2'), None)
            directory.assert_not_called()
            self.assertFalse(panel.show_pending_menu(Gtk.TreePath.new_from_string('2')))
        panel.message.set_text('')
        panel.window.destroy()

    def test_pending_delete_defaults_to_cancel_and_status_details_wrap(self):
        panel = self.panel
        panel.build()
        panel.submit = Mock(return_value=True)
        reason = 'Eligible files synced; some paths need attention: ' + ('a-long-folder/' * 12) + 'file: Unsupported sync path'
        panel.render(dict(paused=False, syncReason=reason), None)
        self.assertEqual(panel.headline.get_text(), 'Some files need attention')
        self.assertEqual(panel.status_details.get_text(), reason)
        self.assertTrue(panel.status_details.get_line_wrap())
        self.assertEqual(int(panel.status_details.get_ellipsize()), 0)
        with patch.object(Gio.File, 'new_for_path') as new_file:
            def dialog():
                return next(w for w in Gtk.Window.list_toplevels() if isinstance(w, Gtk.MessageDialog))
            panel.delete_pending('folder/\\')
            prompt = dialog()
            self.assertEqual(prompt.get_default_widget().get_label(), 'Cancel')
            prompt.response(Gtk.ResponseType.CANCEL)
            new_file.assert_not_called()
            panel.delete_pending('folder/\\')
            dialog().response(Gtk.ResponseType.ACCEPT)
            new_file.assert_called_once_with('/tmp/NASdir/folder/\\')
            file = new_file.return_value
            done = file.trash_async.call_args.args[2]
            file.trash_finish.side_effect = GLib.Error('Permission denied')
            done(file, Mock())
            self.assertIn('Could not move', panel.pending_result.get_text())
            file.trash_finish.side_effect = GLib.Error.new_literal(Gio.io_error_quark(), 'gone', Gio.IOErrorEnum.NOT_FOUND)
            with patch.object(panel, 'load_pending') as refresh:
                done(file, Mock())
                refresh.assert_called_once_with(panel.pending_after)
                self.assertIn('already absent', panel.pending_result.get_text())
            new_file.reset_mock()
            panel.delete_pending('../outside')
            panel.delete_pending('.')
            new_file.assert_not_called()
        panel.window.show_all()
        panel.notebook.set_current_page(panel.notebook.page_num(panel.pending_page))
        panel.submit.call_args.args[1](dict(items=[dict(path='folder/\\', size=21 << 10, action='Upload', reason='Unsupported sync path', confirmable=False, generation=1)], next='', total=1), None)
        panel.message.set_text('')
        panel.pending_result.set_text('')
        while Gtk.events_pending():
            Gtk.main_iteration_do(False)
        import cairo
        allocation = panel.window.get_allocation()
        surface = cairo.ImageSurface(cairo.FORMAT_ARGB32, allocation.width, allocation.height)
        panel.window.draw(cairo.Context(surface))
        surface.write_to_png('/tmp/ananas-pending-status-test.png')
        panel.window.destroy()

    def test_file_notifications_refresh_only_visible_pending_tab_and_coalesce(self):
        panel = self.panel
        panel.build()
        panel.submit = Mock(return_value=True)
        panel.window.show_all()
        panel.notebook.set_current_page(panel.notebook.page_num(panel.pending_page))
        panel.submit.call_args.args[1](dict(items=[], next='', total=0), None)
        with patch.object(GLib, 'timeout_add', return_value=999) as timer, patch.object(panel, 'load_pending') as refresh:
            panel.schedule_pending_refresh()
            panel.schedule_pending_refresh()
            timer.assert_called_once()
            callback = timer.call_args.args[1]
            callback()
            refresh.assert_called_once_with(panel.pending_after)
            timer.reset_mock()
            panel.window.hide()
            panel.schedule_pending_refresh()
            timer.assert_not_called()
        panel.pending_refresh_timer = 0
        panel.window.destroy()

    def test_render_tree_and_failure_recovery(self):
        panel = self.panel
        panel.build()
        panel.window.show_all()
        now = '2026-09-12T17:00:00Z'
        state = dict(paused=False, syncReason='LAN sync active', updatedLast24Hours=12,
                     traffic=dict(session=dict(upload=1024, download=2048),
                                  last24Hours=dict(upload=4096, download=8192),
                                  startedAt=now, recordingSince=now))
        panel.render(state, None)
        self.assertIn('1.0 KiB', panel.session.get_text())
        self.assertIn('8.0 KiB', panel.daily.get_text())
        panel.render(None, 'unavailable')
        self.assertFalse(panel.pause.get_sensitive())
        panel.render(state, None)
        self.assertTrue(panel.pause.get_sensitive())
        usage = dict(name='NASdir', path='', localBytes=100, syncedBytes=80, files=2,
                     children=[dict(name='SampleDocuments', path='SampleDocuments', localBytes=100, syncedBytes=80, files=2)])
        panel.storage_done(dict(capacity=dict(available=True, free=900, used=100, total=1000, checkedAt=now), usage=usage), None)
        self.assertEqual(panel.capacity_bar.get_fraction(), 0.1)
        self.assertEqual(panel.model[0][0], 'NASdir')
        self.assertEqual(panel.model['0:0'][0], 'SampleDocuments')
        hidden = dict(name='.ananas-tests', path='.ananas-tests', localBytes=0, syncedBytes=0, files=0)
        usage['children'].append(hidden)
        panel.storage_done(dict(capacity=dict(available=False), usage=usage), None)
        self.assertEqual(panel.model.iter_n_children(panel.model.get_iter_first()), 1)
        self.assertEqual(panel.free.get_text(), 'Space unavailable')
        # Expanding a directory enqueues work instead of doing I/O in GTK.
        panel.submit = Mock(return_value=True)
        panel.tree.expand_row(Gtk.TreePath.new_from_string('0:0'), False)
        panel.submit.assert_called_once()
        done = panel.submit.call_args.args[1]
        sub = dict(name='sub folder', path='SampleDocuments/sub folder', localBytes=100, syncedBytes=80, files=2)
        done({'usage': dict(usage, name='SampleDocuments', path='SampleDocuments', children=[sub])}, None)
        self.assertTrue(panel.tree.row_expanded(Gtk.TreePath.new_from_string('0:0')),
                        'first expansion collapsed when the loading child was replaced')
        panel.tree.expand_row(Gtk.TreePath.new_from_string('0:0:0'), False)
        done = panel.submit.call_args.args[1]
        panel.tree.collapse_row(Gtk.TreePath.new_from_string('0:0:0'))
        done({'usage': dict(sub, children=[dict(sub, name='nested', path='SampleDocuments/sub folder/nested')])}, None)
        self.assertFalse(panel.tree.row_expanded(Gtk.TreePath.new_from_string('0:0:0')),
                         'a completed request reopened a folder the user had collapsed')
        with patch.object(Gio.AppInfo, 'launch_default_for_uri_async') as launch:
            panel.tree.emit('row-activated', Gtk.TreePath.new_from_string('0:0:0'), panel.tree.get_column(0))
            self.assertEqual(launch.call_args.args[0], 'file:///tmp/NASdir/SampleDocuments/sub%20folder')
            launch.reset_mock()
            panel.open_directory('../outside')
            launch.assert_not_called()
        panel.directory_error(Path('/tmp/NASdir/SampleDocuments'), GLib.Error('raw failure'))
        self.assertTrue(panel.connected)
        self.assertTrue(panel.pause.get_sensitive())
        self.assertNotIn('raw failure', panel.message.get_text())
        panel.message.set_text('')
        with patch.object(Gtk.Menu, 'popup_at_rect'), \
                patch.object(panel, 'open_directory') as open_directory, \
                patch.object(panel, 'open_terminal') as open_terminal:
            self.assertTrue(panel.show_folder_menu(Gtk.TreePath.new_from_string('0:0:0')))
            actions = panel.folder_menu.get_children()
            self.assertEqual([item.get_label() for item in actions], ['Open directory', 'Open terminal here'])
            text_color = actions[0].get_child().get_style_context().get_color(Gtk.StateFlags.NORMAL)
            background = panel.folder_menu.get_style_context().get_background_color(Gtk.StateFlags.NORMAL)
            self.assertLess(text_color.red, 0.2)
            self.assertGreater(background.red, 0.95)
            actions[0].activate()
            actions[1].activate()
            open_directory.assert_called_once_with('SampleDocuments/sub folder')
            open_terminal.assert_called_once_with('SampleDocuments/sub folder')
        with patch.object(Gio.Subprocess, 'new') as launch:
            panel.open_terminal('SampleDocuments/sub folder/$HOME')
            argv = launch.call_args.args[0]
            self.assertNotIn('--expand-environment=no', argv)
            self.assertEqual(argv[-2:], ['/usr/bin/xdg-terminal-exec', '--dir=/tmp/NASdir/SampleDocuments/sub folder/$$HOME'])
            launch.return_value.wait_check_async.assert_called_once()
            launch.reset_mock()
            panel.open_terminal('../outside')
            launch.assert_not_called()
        panel.message.set_text('')
        while Gtk.events_pending():
            Gtk.main_iteration_do(False)
        # Render our own widget directly, without capturing other desktop apps.
        import cairo
        allocation = panel.window.get_allocation()
        surface = cairo.ImageSurface(cairo.FORMAT_ARGB32, allocation.width, allocation.height)
        panel.window.draw(cairo.Context(surface))
        surface.write_to_png('/tmp/ananas-control-panel-widget-test.png')
        panel.window.destroy()

    def test_storage_failure_preserves_live_status_and_recovers(self):
        panel = self.panel
        panel.last_capacity_text = None
        panel.usage_summary = None
        panel.storage_error = None
        panel.build()
        state = dict(paused=False, syncReason='Synchronizing', activePath='research/scratch/s.wfc')
        panel.render(state, None)
        panel.storage_done(None, 'Storage summary failed (HTTP 503). Local index time limit.')
        self.assertTrue(panel.connected)
        self.assertTrue(panel.pause.get_sensitive())
        self.assertEqual(panel.headline.get_text(), 'Synchronizing')
        self.assertIn(state['activePath'], panel.status_details.get_text())
        self.assertIn('HTTP 503', panel.message.get_text())
        self.assertIn('independently', panel.message.get_text())
        self.assertEqual(panel.free.get_text(), 'Space unknown')
        self.assertEqual(panel.tree_summary.get_text(), 'Folder sizes unavailable')
        panel.render(None, 'connection refused')
        self.assertIn('cannot be confirmed', panel.message.get_text())
        self.assertIn('connection refused', panel.message.get_text())
        self.assertIn('Synchronizing', panel.status_details.get_text())
        panel.storage_done(None, 'storage timed out')
        self.assertIn('cannot be confirmed', panel.message.get_text())
        panel.render(state, None)
        self.assertIn('storage timed out', panel.message.get_text())
        usage = dict(name='NASdir', path='', localBytes=100, files=2, children=[])
        data = dict(capacity=dict(available=True, free=900, used=100, total=1000, checkedAt='2026-11-10T16:00:00Z'), usage=usage)
        panel.storage_done(data, None)
        self.assertEqual(panel.message.get_text(), '')
        self.assertIn('available', panel.free.get_text())
        original_summary = panel.tree_summary.get_text()
        original_rows = len(panel.model)
        panel.storage_done(dict(capacity=data['capacity'], usageError='Local folder index time limit.'), None)
        self.assertEqual(panel.free.get_text(), '900.0 B available')
        self.assertEqual(len(panel.model), original_rows)
        self.assertEqual(panel.tree_summary.get_text(), original_summary + ' · out of date')
        self.assertIn('Folder sizes unavailable', panel.message.get_text())
        panel.render(state, None)
        self.assertIn('Folder sizes unavailable', panel.message.get_text())
        panel.storage_done(data, None)
        self.assertEqual(panel.message.get_text(), '')
        self.assertEqual(panel.tree_summary.get_text(), original_summary)
        panel.storage_done(None, 'request timed out')
        self.assertIn('Last known: 900.0 B', panel.free.get_text())
        panel.window.destroy()


if __name__ == '__main__':
    unittest.main()
