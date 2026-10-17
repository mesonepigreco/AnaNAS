import unittest
from pathlib import Path
from unittest.mock import Mock, patch
from ananas_control_panel import Panel, Gtk, Gio
from ananas_problems import choice_explanation, remote_action
from nas_sync_indicator import Indicator


class ProblemTests(unittest.TestCase):
    def test_explanations_distinguish_delete_and_update(self):
        removed = {'remote': {'tombstone': True}}
        self.assertIn('renamed', remote_action(removed))
        self.assertIn('absent', choice_explanation('nas', removed, '/recovery'))
        self.assertIn('No NAS files are changed', choice_explanation('nas', removed, '/recovery'))
        self.assertIn('rebuilt', choice_explanation('nas', {'remote': {}}, '/recovery'))
        self.assertIn('suspended', choice_explanation('separate', removed, '/recovery'))

    def test_notification_opt_out_and_dedup_leave_resolution_available(self):
        indicator = Indicator.__new__(Indicator)
        indicator.bus = Mock()
        indicator.problem_notice = None
        indicator.problem_count = 0
        indicator.problem_notification_id = None
        indicator.state = {'syncProblems': 2, 'syncProblemsTotal': 2, 'syncProblemSignature': 'first', 'notifySyncProblems': False}
        indicator.notify_sync_problems()
        indicator.bus.call.assert_not_called()
        self.assertEqual(indicator.icon(), 'ananas-broken')
        indicator.state['notifySyncProblems'] = True
        indicator.notify_sync_problems()
        indicator.bus.call.assert_not_called()
        indicator.state.update(syncProblems=3, syncProblemSignature='new')
        indicator.notify_sync_problems()
        indicator.notify_sync_problems()
        self.assertEqual(indicator.bus.call.call_count, 1)
        indicator.state.update(syncProblems=2, syncProblemSignature='fewer')
        indicator.notify_sync_problems()
        self.assertEqual(indicator.bus.call.call_count, 1)
        indicator.state['syncProblems'] = 0
        self.assertNotEqual(indicator.icon(), 'ananas-broken')

    def test_dialog_cancel_stale_choice_and_settings(self):
        if not Gtk.init_check()[0]: self.skipTest('GTK display unavailable')
        panel = Panel(Mock(), Path('/tmp/test-sync'))
        panel.set_application_id('org.ananas.Problems.Test')
        panel.set_flags(Gio.ApplicationFlags.NON_UNIQUE)
        panel.register(None)
        panel.build()
        panel.submit = Mock(return_value=True)
        panel.state = {'notifySyncProblems': True, 'recoveryDirectory': '/tmp/recovery'}
        panel.problems.show()
        item = {'path': 'old/folder', 'revision': 'revision', 'remote': {'tombstone': True}, 'reason': 'Directory identity missing'}
        panel.submit.call_args.args[1]({'items': [item], 'total': 1, 'attention': 1, 'next': ''}, None)
        panel.problems.tree.get_selection().select_path(Gtk.TreePath.new_from_string('0'))
        panel.submit.reset_mock()
        with patch('ananas_problems.Gtk.MessageDialog') as dialog:
            dialog.return_value.run.return_value = Gtk.ResponseType.CANCEL
            panel.problems.choose('nas')
            dialog.return_value.set_default_response.assert_called_with(Gtk.ResponseType.CANCEL)
            panel.submit.assert_not_called()
            dialog.return_value.run.return_value = Gtk.ResponseType.ACCEPT
            panel.problems.choose('nas')
        work, done = panel.submit.call_args.args
        work()
        panel.client.resolve_sync.assert_called_with('old/folder', 'revision', 'nas')
        done(None, 'stale')
        self.assertIn('changed', panel.problems.result.get_text())
        panel.notify_problems.set_active(True)
        work, done = panel.submit.call_args.args
        work()
        panel.client.set_problem_notifications.assert_called_with(True)
        done({'saved': True}, None)
        panel.notify_problems.set_active(False)
        work, done = panel.submit.call_args.args
        work()
        panel.client.set_problem_notifications.assert_called_with(False)
        done({'saved': True}, None)
        self.assertFalse(panel.state['notifySyncProblems'])
        panel.problems.close()
        panel.window.destroy()


if __name__ == '__main__': unittest.main()
