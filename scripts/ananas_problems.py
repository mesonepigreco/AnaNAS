"""Decision dialog for retained incoming changes; no filesystem mutations in GTK."""
from pathlib import Path
from gi.repository import Gtk, GLib, Pango, Gio


def remote_action(item):
    version = item.get('remote', {})
    if version.get('tombstone'):
        return 'Removed on NAS (possibly moved or renamed)'
    return 'Folder from NAS' if version.get('directory') else 'Updated file from NAS'


def choice_explanation(choice, item, recovery):
    if choice == 'retry':
        return 'Try the saved NAS change again using the normal safety checks. No version is forced. If the problem remains, it stays in this list.'
    if choice == 'separate':
        return 'Keep this computer’s copy and the NAS copy different. Automatic uploads and downloads are suspended for this path and its contents. Other folders continue syncing. You can return here to resume.'
    change = ('The original path will be absent on this computer, matching the NAS. Any new name from a remote rename syncs separately.'
              if item.get('remote', {}).get('tombstone') else
              'This path and its contents will be rebuilt from the latest NAS versions already received by this computer.')
    return ('First save the current local item and all its contents outside the synchronized folder. ' + change +
            '\n\nNo NAS files are changed by this action. Local files saved for recovery are kept until you remove them.\nRecovery copies: ' + recovery)


class Problems:
    def __init__(self, panel):
        self.panel = panel
        self.dialog = None
        self.loading = False
        self.after = ''
        self.next = ''
        self.items = {}

    def show(self):
        if self.dialog is not None:
            self.dialog.present()
            self.refresh()
            return
        self.dialog = Gtk.Dialog(title='Resolve sync problems', transient_for=self.panel.window)
        self.dialog.get_style_context().add_class('recovery-dialog')
        self.dialog.set_default_size(960, 620)
        self.dialog.add_button('Close', Gtk.ResponseType.CLOSE)
        self.dialog.set_default_response(Gtk.ResponseType.CLOSE)
        self.dialog.connect('response', lambda *_: self.close())
        self.dialog.connect('delete-event', lambda *_: self.close())
        box = self.dialog.get_content_area()
        box.set_spacing(12)
        box.set_border_width(18)
        title = self.panel.label('Your local files are waiting for a decision', 'section')
        box.pack_start(title, False, False, 0)
        note = self.panel.label('A change from another computer or the NAS could not be applied safely.\nThe received NAS version is retained. Unrelated paths continue syncing.', 'muted')
        note.set_line_wrap(True)
        box.pack_start(note, False, False, 0)
        self.model = Gtk.ListStore(str, str, str)
        self.tree = Gtk.TreeView(model=self.model)
        for i, title in enumerate(('File or folder', 'NAS change', 'State')):
            cell = Gtk.CellRendererText()
            cell.set_property('ellipsize', Pango.EllipsizeMode.END)
            col = Gtk.TreeViewColumn(title, cell, text=i)
            col.set_min_width((280, 240, 150)[i])
            col.set_expand(i == 0)
            col.set_resizable(True)
            self.tree.append_column(col)
        self.tree.set_tooltip_column(0)
        self.tree.get_selection().connect('changed', lambda *_: self.selection())
        scroll = Gtk.ScrolledWindow()
        scroll.set_min_content_height(170)
        scroll.add(self.tree)
        box.pack_start(scroll, True, True, 0)
        self.detail = self.panel.label('Select a path to review the choices.')
        self.detail.set_line_wrap(True)
        self.detail.set_line_wrap_mode(Pango.WrapMode.WORD_CHAR)
        self.detail.set_max_width_chars(85)
        box.pack_start(self.detail, False, False, 0)
        actions = Gtk.Box(spacing=8)
        self.buttons = []
        for label, choice in [('Try again safely', 'retry'), ('Save local copy and follow NAS…', 'nas'), ('Keep versions separate…', 'separate')]:
            button = Gtk.Button(label=label)
            button.connect('clicked', lambda _, c=choice: self.choose(c))
            actions.pack_start(button, False, False, 0)
            self.buttons.append(button)
        box.pack_start(actions, False, False, 0)
        navigation = Gtk.Box(spacing=8)
        for label, work in [('Refresh', lambda: self.refresh()), ('First page', lambda: self.refresh('')), ('Next page', lambda: self.refresh(self.next)), ('Open recovery copies', self.open_recovery)]:
            button = Gtk.Button(label=label)
            button.connect('clicked', lambda _, f=work: f())
            navigation.pack_start(button, False, False, 0)
            if label == 'Next page': self.more = button
        box.pack_start(navigation, False, False, 0)
        self.result = self.panel.label('', 'muted')
        self.result.set_line_wrap(True)
        box.pack_start(self.result, False, False, 0)
        self.dialog.show_all()
        self.selection()
        self.refresh('')

    def close(self):
        if self.dialog is not None:
            self.dialog.destroy()
            self.dialog = None
        return True

    def selected(self):
        model, node = self.tree.get_selection().get_selected()
        return self.items.get(model[node][0]) if node is not None else None

    def selection(self):
        item = self.selected()
        for button in self.buttons:
            button.set_sensitive(bool(item) and not item.get('request') and not self.loading)
        if item:
            self.detail.set_text(item['path'] + '\n' + remote_action(item) + '\n' + item['reason'])

    def refresh(self, after=None):
        if self.dialog is None or self.loading:
            return
        self.loading = True
        cursor = self.after if after is None else after
        self.selection()
        def done(value, error):
            self.loading = False
            if self.dialog is None: return False
            if error:
                self.result.set_text('Could not load problems. Refresh to try again.')
            else:
                selected = self.selected()
                self.items = {item['path']: item for item in value['items']}
                self.model.clear()
                for item in value['items']:
                    state = 'Recovery queued' if item.get('request') else 'Kept separate' if item.get('choice') == 'separate' else 'Needs a decision'
                    node = self.model.append([item['path'], remote_action(item), state])
                    if selected and item['path'] == selected['path']:
                        self.tree.get_selection().select_iter(node)
                self.after, self.next = cursor, value.get('next', '')
                self.more.set_sensitive(bool(self.next))
                self.result.set_text(f"{value['total']} paths · {value['attention']} need a decision")
                if not value['items']: self.detail.set_text('No sync problems on this page. Local recovery copies are kept separately.')
            self.selection()
            return False
        if not self.panel.submit(lambda: self.panel.client.sync_problems(cursor), done):
            self.loading = False
            self.selection()

    def choose(self, choice):
        item = self.selected()
        if not item or item.get('request'): return
        recovery = (self.panel.state or {}).get('recoveryDirectory', 'the private local recovery folder')
        dialog = Gtk.MessageDialog(transient_for=self.dialog, modal=True, message_type=Gtk.MessageType.QUESTION, buttons=Gtk.ButtonsType.NONE,
                                   text={'retry': 'Try this NAS change again?', 'nas': 'Save a local copy and follow the NAS?', 'separate': 'Suspend sync for this path?'}[choice])
        dialog.get_style_context().add_class('recovery-dialog')
        dialog.format_secondary_text(item['path'] + '\n\n' + choice_explanation(choice, item, recovery))
        dialog.add_button('Cancel', Gtk.ResponseType.CANCEL)
        dialog.add_button({'retry': 'Try again', 'nas': 'Save copy and follow NAS', 'separate': 'Keep separate'}[choice], Gtk.ResponseType.ACCEPT)
        dialog.set_default_response(Gtk.ResponseType.CANCEL)
        response = dialog.run()
        dialog.destroy()
        if response != Gtk.ResponseType.ACCEPT: return
        self.loading = True
        self.selection()
        def done(value, error):
            self.loading = False
            if self.dialog is None: return False
            self.result.set_text('This problem changed or the request failed. Refresh and review again.' if error else 'Choice saved. Recovery runs when synchronization is active.')
            self.refresh()
            return False
        if not self.panel.submit(lambda: self.panel.client.resolve_sync(item['path'], item['revision'], choice), done):
            self.loading = False
            self.selection()

    def open_recovery(self):
        directory = (self.panel.state or {}).get('recoveryDirectory')
        if directory:
            Gio.AppInfo.launch_default_for_uri_async(Path(directory).as_uri(), None, None,
                lambda source, result: self.panel.directory_opened(result, Path(directory)))
