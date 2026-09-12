import concurrent.futures
from pathlib import Path
import unittest
from unittest.mock import patch, Mock

from ananas_setup_gui import Wizard, Gtk, Gio, GLib
import ananas_setup as backend


class WizardTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if not Gtk.init_check()[0]:
            raise unittest.SkipTest('GTK display unavailable')

    def test_login_folder_choices_and_failure_state(self):
        app = Gtk.Application(application_id='org.ananas.Setup.WidgetTest', flags=Gio.ApplicationFlags.NON_UNIQUE)
        app.register(None)
        with patch.object(backend, 'profiles', return_value=[]):
            wizard = Wizard(app)
        try:
            self.assertFalse(wizard.password.get_visibility())
            self.assertEqual(wizard.stack.get_visible_child_name(), 'connect')
            wizard.found([dict(host='10.23.42.30', name='My QNAP')])
            self.assertEqual(wizard.host.get_text(), '10.23.42.30')
            self.assertIn('My QNAP', wizard.nas_name.get_text())
            self.assertIn('10.23.42.30', wizard.nas_name.get_text())
            wizard.host.set_text('10.23.42.31')
            self.assertNotIn('My QNAP', wizard.nas_name.get_text())
            old = concurrent.futures.Future()
            old.set_result(dict(host='10.23.42.30', name='Old address QNAP'))
            wizard.name_ready('10.23.42.30', old)
            self.assertNotIn('Old address QNAP', wizard.nas_name.get_text())
            matching = concurrent.futures.Future()
            matching.set_result(dict(host='10.23.42.31', name='Second QNAP'))
            wizard.name_ready('10.23.42.31', matching)
            self.assertIn('Second QNAP', wizard.nas_name.get_text())
            wizard.host.set_text('10.23.42.30')
            self.assertIn('My QNAP', wizard.nas_name.get_text())
            self.assertIn('QNAP NAS', wizard.status.get_text())
            wizard.session = Mock(host='10.23.42.30')
            wizard.folders_page(dict(model='QNAP test fixture', shares=[dict(name='Public', path='/share/volume/Public'), dict(name='SampleDocuments', path='/share/volume/SampleDocuments')],
                                     accounts=[dict(name='researcher', uid=1000, gid=100)], managed=[dict(root='/share/volume/SampleDocuments', peers=['10.23.42.5'])]))
            self.assertEqual(wizard.folder_model[0][0], 'SampleDocuments')
            self.assertIn('Configured with anaNAS', wizard.folder_model[0][1])
            self.assertEqual(wizard.folder_model[0][7], 'folder-remote-symbolic')
            self.assertEqual(wizard.folder_model[0][8], 'ananas-symbolic')
            self.assertEqual(wizard.folder_model[1][0], 'Public')
            self.assertEqual(wizard.folder_model[1][8], '')
            self.assertTrue(wizard.next_button.get_sensitive())
            self.assertEqual(wizard.stack.get_visible_child_name(), 'folders')
            wizard.session.browse.return_value = dict(entries=[dict(name='Slides', relative='Slides', path='/share/volume/SampleDocuments/Slides', directory=True, kind='directory', bytes=0),
                                                               dict(name='paper.pdf', relative='paper.pdf', path='/share/volume/SampleDocuments/paper.pdf', directory=False, kind='file', bytes=4096)], truncated=False)
            wizard.folder_tree.expand_row(Gtk.TreePath.new_from_string('0'), False)
            loop = GLib.MainLoop()
            GLib.timeout_add(150, lambda: (loop.quit(), False)[1])
            loop.run()
            self.assertTrue(wizard.folder_tree.row_expanded(Gtk.TreePath.new_from_string('0')))
            self.assertEqual(wizard.folder_model['0:0'][0], 'Slides')
            self.assertEqual(wizard.folder_model['0:0'][7], 'folder-symbolic')
            self.assertEqual(wizard.folder_model['0:1'][7], 'text-x-generic-symbolic')
            wizard.folder_tree.get_selection().select_path(Gtk.TreePath.new_from_string('0:1'))
            self.assertFalse(wizard.next_button.get_sensitive())
            wizard.folder_tree.get_selection().select_path(Gtk.TreePath.new_from_string('1'))
            with patch.object(backend, 'profiles', return_value=[]):
                wizard.continue_folder(None)
            self.assertEqual(wizard.stack.get_visible_child_name(), 'destination')
            self.assertEqual(wizard.account.get_active_text(), 'researcher')
            self.assertTrue(wizard.save.get_active())
            self.assertEqual(wizard.chosen_share['name'], 'Public')
            failure = concurrent.futures.Future()
            failure.set_exception(backend.SetupError('The LAN disconnected. Retry when connected.'))
            wizard.busy = True
            wizard.installing = True
            wizard.finished(failure, lambda _: self.fail('unexpected success'))
            self.assertFalse(wizard.busy)
            self.assertFalse(wizard.installing)
            self.assertTrue(wizard.stack.get_sensitive())
            self.assertIn('LAN disconnected', wizard.status.get_text())
            wizard.stack.set_visible_child_name('folders')
            wizard.status.set_text('Test view — no connection or installation performed.')
            loop = GLib.MainLoop()
            GLib.timeout_add(350, lambda: (loop.quit(), False)[1])
            loop.run()
            while Gtk.events_pending():
                Gtk.main_iteration_do(False)
            import cairo
            allocation = wizard.get_allocation()
            surface = cairo.ImageSurface(cairo.FORMAT_ARGB32, allocation.width, allocation.height)
            wizard.draw(cairo.Context(surface))
            surface.write_to_png('/tmp/ananas-setup-widget-test.png')
        finally:
            wizard.close_requested()
            wizard.destroy()


if __name__ == '__main__':
    unittest.main()
