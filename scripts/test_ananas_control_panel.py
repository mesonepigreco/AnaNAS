"""Native widget regression checks; run with an available desktop display."""
import unittest
from pathlib import Path
from unittest.mock import Mock, patch
from ananas_control_panel import Panel, Gtk, GLib, Gio, size


class PanelTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if not Gtk.init_check()[0]:
            raise unittest.SkipTest("GTK display unavailable")

    def test_render_tree_and_failure_recovery(self):
        panel = Panel(Mock(), Path('/tmp/NASdir'))
        panel.set_application_id('org.ananas.ControlPanel.WidgetTest')
        panel.register(None)
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
        self.assertEqual(panel.free.get_text(), 'NAS unavailable')
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
            self.assertIn('--expand-environment=no', argv)
            self.assertEqual(argv[-2:], ['/usr/bin/xdg-terminal-exec', '--dir=/tmp/NASdir/SampleDocuments/sub folder/$HOME'])
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


if __name__ == '__main__':
    unittest.main()
