import unittest
from unittest.mock import Mock
from ananas_nas_tree import NASTree, Gtk, GLib, contains


class PathTests(unittest.TestCase):
    def test_active_directory_boundaries(self):
        self.assertTrue(contains('Research', 'Research/file'))
        self.assertTrue(contains('', 'Research/file'))
        self.assertFalse(contains('Research', 'Research-old/file'))
        self.assertFalse(contains('', ''))


class NASTreeTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if not Gtk.init_check()[0]:
            raise unittest.SkipTest('GTK display unavailable')

    def test_remote_only_progress_activity_priority_and_preserved_expansion(self):
        panel = Mock()
        panel.submit.return_value = True
        tree = NASTree(panel)
        window = Gtk.Window(title='anaNAS NAS tree test')
        window.set_default_size(960, 500)
        window.add(tree)
        window.show_all()
        root = dict(path='', name='NAS', directory=True, files=10, syncedFiles=3,
                    inventory={'complete': True}, priority='', loading=False, problem='', activePath='Research/paper.pdf', next='',
                    children=[dict(path='Research', name='Research', directory=True, files=8, syncedFiles=2),
                              dict(path='Photos', name='Photos', directory=True, files=2, syncedFiles=1)])
        try:
            panel.submit.call_args.args[1](root, None)
            research = tree.find('Research')
            self.assertTrue(tree.model[research][5])
            self.assertFalse(tree.model[tree.find('Photos')][5])
            self.assertEqual(tree.model[research][3:5], [8, 2])
            tree.view.expand_row(tree.model.get_path(research), False)
            children = dict(root, path='Research', name='Research', files=8, syncedFiles=2,
                            children=[dict(path='Research/paper.pdf', name='paper.pdf', directory=False, files=1, syncedFiles=0)])
            panel.submit.call_args.args[1](children, None)
            tree.load('')
            panel.submit.call_args.args[1](root, None)
            self.assertIsNotNone(tree.find('Research/paper.pdf'))
            self.assertTrue(tree.view.row_expanded(tree.model.get_path(tree.find('Research'))))
            tree.prioritize('Research')
            work, done = panel.submit.call_args.args
            work()
            panel.client.prioritize_directory.assert_called_once_with('Research')
            done({'queued': True}, None)
            self.assertIn('current transfer', tree.message.get_text())
            panel.client.pause.assert_not_called()
            tree.update_activity('Photos/holiday.jpg')
            self.assertFalse(tree.model[tree.find('Research')][5])
            self.assertTrue(tree.model[tree.find('Photos')][5])
            loop = GLib.MainLoop()
            GLib.timeout_add(150, lambda: (loop.quit(), False)[1])
            loop.run()
            import cairo
            a = window.get_allocation()
            surface = cairo.ImageSurface(cairo.FORMAT_ARGB32, a.width, a.height)
            window.draw(cairo.Context(surface))
            surface.write_to_png('/tmp/ananas-nas-tree-test.png')
            tree.update_activity('')
            self.assertEqual(tree.timer, 0)
        finally:
            window.destroy()
