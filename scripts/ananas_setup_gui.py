#!/usr/bin/python3
"""Native anaNAS setup wizard. Network and installation work never blocks GTK."""
import concurrent.futures
import ipaddress
from pathlib import Path
import subprocess

import gi
gi.require_version("Gtk", "3.0")
from gi.repository import Gio, GLib, Gtk

import ananas_setup as backend


class Wizard(Gtk.ApplicationWindow):
    def __init__(self, app):
        super().__init__(application=app, title="anaNAS — Set up synchronization")
        self.set_default_size(850, 650)
        self.set_border_width(22)
        self.executor = concurrent.futures.ThreadPoolExecutor(max_workers=1)
        self.session = None
        self.inventory = None
        self.busy = False
        self.installing = False
        self.closed = False
        self.known_devices = {}
        self.identity_timer = 0
        self.identity_future = None
        self.connect("delete-event", self.close_requested)
        outer = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=14)
        self.add(outer)
        title = Gtk.Label(xalign=0)
        title.set_markup('<span size="x-large" weight="bold">Choose QNAP folders to sync</span>')
        outer.pack_start(title, False, False, 0)
        intro = Gtk.Label(label="Sign in to see your NAS shared folders, browse their contents, and choose what to sync on this PC.", xalign=0, wrap=True)
        self.intro = intro
        outer.pack_start(intro, False, False, 0)
        self.stack = Gtk.Stack(transition_type=Gtk.StackTransitionType.SLIDE_LEFT_RIGHT)
        self.stack.set_vhomogeneous(False)
        self.stack.connect('notify::visible-child-name', self.page_changed)
        outer.pack_start(self.stack, True, True, 0)
        self.status = Gtk.Label(xalign=0, wrap=True, selectable=True)
        outer.pack_start(self.status, False, False, 0)
        self.spinner = Gtk.Spinner()
        outer.pack_start(self.spinner, False, False, 0)
        self.login_page()
        self.existing_page()
        self.show_all()
        self.stack.set_visible_child_name("connect")
        self.status.set_text("Choose Find QNAP on LAN, or enter your QNAP's IP address.")

    def page_changed(self, stack, _):
        messages = {
            'connect': 'Sign in to see your NAS shared folders, browse their contents, and choose what to sync on this PC.',
            'folders': 'Browse all accessible shared folders. Configured anaNAS folders appear first; ordinary folders can be selected too.',
            'destination': 'Choose where to save the selected NAS folder on this PC. You will review the setup before anything changes.',
            'existing': 'Manage the folders already configured on this PC. Stopping sync does not delete files.',
        }
        self.intro.set_text(messages.get(stack.get_visible_child_name(), messages['connect']))

    def box(self, name):
        box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=12)
        self.stack.add_named(box, name)
        return box

    def label(self, box, text):
        label = Gtk.Label(label=text, xalign=0, wrap=True)
        box.pack_start(label, False, False, 0)
        return label

    def button(self, box, text, callback):
        button = Gtk.Button(label=text)
        button.connect("clicked", callback)
        box.pack_start(button, False, False, 0)
        return button

    def field(self, box, label, text="", secret=False):
        self.label(box, label)
        entry = Gtk.Entry(text=text)
        entry.set_visibility(not secret)
        entry.set_input_purpose(Gtk.InputPurpose.PASSWORD if secret else Gtk.InputPurpose.FREE_FORM)
        box.pack_start(entry, False, False, 0)
        return entry

    def login_page(self):
        box = self.box("connect")
        current = backend.profiles()
        if current:
            self.button(box, f"Manage {len(current)} configured local sync folder(s)…", lambda _: self.show_existing())
        self.host = self.field(box, "QNAP LAN address", current[0]["nas"]["host"] if current else "")
        self.nas_name = self.label(box, 'NAS name will appear here after discovery.')
        self.nas_name.set_selectable(True)
        self.host.connect('changed', self.host_changed)
        self.candidates = Gtk.ComboBoxText()
        self.candidates.connect("changed", lambda widget: self.host.set_text(widget.get_active_id() or self.host.get_text()))
        box.pack_start(self.candidates, False, False, 0)
        self.button(box, "Find QNAP on LAN", lambda _: self.work(backend.discover, self.found, "Looking for QNAP NAS devices on your LAN…"))
        self.username = self.field(box, "QNAP administrator username")
        self.password = self.field(box, "QNAP password (used only for this setup session)", secret=True)
        self.password.connect("activate", self.connect_nas)
        self.label(box, "SSH must be enabled in QTS. The helper runs as a separate non-admin account.\nYour NAS login is separate from the PC administrator confirmation for mounting/autostart.")
        self.button(box, "Connect", self.connect_nas)
        self.host_changed(self.host)

    def host_changed(self, entry):
        if self.identity_timer:
            GLib.source_remove(self.identity_timer)
            self.identity_timer = 0
        host = entry.get_text().strip()
        device = self.known_devices.get(host)
        if device:
            self.nas_name.set_text('QNAP: ' + device['name'] + ' · ' + host)
            return
        self.nas_name.set_text('NAS name not verified for this address.')
        try:
            ipaddress.IPv4Address(host)
        except ValueError:
            return
        self.nas_name.set_text('Looking up the QNAP name…')
        self.identity_timer = GLib.timeout_add(450, self.lookup_name, host)

    def lookup_name(self, host):
        self.identity_timer = 0
        if self.closed or self.host.get_text().strip() != host:
            return False
        if self.busy or (self.identity_future and not self.identity_future.done()):
            self.identity_timer = GLib.timeout_add(450, self.lookup_name, host)
            return False
        self.identity_future = self.executor.submit(backend.identify_qnap, host)
        self.identity_future.add_done_callback(lambda future: GLib.idle_add(self.name_ready, host, future))
        return False

    def name_ready(self, host, future):
        if self.closed or self.host.get_text().strip() != host:
            return False  # Never show another address's delayed response.
        try:
            device = future.result()
        except Exception:
            device = None
        if device:
            self.known_devices[host] = device
            self.nas_name.set_text('QNAP: ' + device['name'] + ' · ' + host)
        else:
            self.nas_name.set_text('QNAP name unavailable. Check this address before connecting.')
        return False

    def existing_page(self):
        old = self.stack.get_child_by_name("existing")
        if old:
            self.stack.remove(old)
            old.destroy()
        box = self.box("existing")
        current = backend.profiles()
        self.label(box, f"{len(current)} configured local folder(s). Stopping sync never removes files.")
        scroll = Gtk.ScrolledWindow()
        scroll.set_policy(Gtk.PolicyType.NEVER, Gtk.PolicyType.AUTOMATIC)
        items = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=12)
        scroll.add(items)
        box.pack_start(scroll, True, True, 0)
        for profile in current:
            frame = Gtk.Frame(label=profile["nas"]["host"] + " / " + profile["nas"]["share"])
            row = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=6, margin=10)
            frame.add(row)
            self.label(row, profile["local"]["root"])
            self.button(row, "Open control panel", lambda _, p=profile: self.open_panel(p))
            self.button(row, "Enable and start sync", lambda _, p=profile: self.service(p, True))
            self.button(row, "Stop sync and disable autostart", lambda _, p=profile: self.service(p, False))
            items.pack_start(frame, False, False, 0)
        self.button(box, "Add another folder…", self.add_another)
        box.show_all()

    def show_existing(self):
        self.existing_page()
        self.stack.set_visible_child_name("existing")
        self.work(lambda: [(p, subprocess.run(["systemctl", "--user", "is-active", p["_service"]], capture_output=True, text=True, timeout=5).stdout.strip())
                           for p in backend.profiles()],
                  lambda states: self.status.set_text("\n".join(f"{p['local']['root']}: {state or 'not running'}" for p, state in states)),
                  "Checking configured services…")

    def add_another(self, _):
        if self.session:
            self.work(self.session.inventory, self.folders_page, "Refreshing folders with your existing login…")
        else:
            self.stack.set_visible_child_name("connect")

    def service(self, profile, start):
        record = Path(profile["_config"]).with_name("setup.json")
        if start and record.exists():
            import json
            if json.loads(record.read_bytes()).get("phase") != "started":
                self.status.set_text("This setup is incomplete. Review its recovery record before starting: " + str(record))
                return
        self.work(lambda: backend.run(["systemctl", "--user", "enable" if start else "disable", "--now", profile["_service"]]),
                  lambda _: self.status.set_text("Sync enabled and started." if start else "Sync stopped; local and NAS files are unchanged."),
                  "Updating this folder's service…")

    def open_panel(self, profile):
        try:
            Gio.Subprocess.new(["/usr/bin/python3", str(Path.home() / ".local/bin/nas-sync-indicator"), "--control-panel",
                                "--port", str(profile["webPort"]), "--root", profile["local"]["root"]], Gio.SubprocessFlags.NONE)
        except GLib.Error:
            self.status.set_text("Could not open the control panel. Run the installer to repair the desktop components.")

    def work(self, operation, done, message):
        if self.busy:
            return
        self.busy = True
        self.stack.set_sensitive(False)
        self.spinner.start()
        self.status.set_text(message)
        future = self.executor.submit(operation)
        future.add_done_callback(lambda future: GLib.idle_add(self.finished, future, done))

    def finished(self, future, done):
        if self.closed:
            return False
        self.busy = False
        self.stack.set_sensitive(True)
        self.spinner.stop()
        try:
            done(future.result())
        except backend.SetupError as error:
            self.status.set_text(str(error))
            self.installing = False
        except Exception:
            # Do not display exceptions containing remote output or login data.
            self.status.set_text("Could not complete this step. Check the NAS connection and try again. Existing sync folders were not replaced.")
            self.installing = False
        return False

    def found(self, devices):
        preferred = self.host.get_text()
        self.known_devices.update({device['host']: device for device in devices})
        self.candidates.remove_all()
        hosts = [item['host'] for item in devices]
        for device in devices:
            self.candidates.append(device['host'], device['name'] + ' — ' + device['host'])
        if hosts:
            self.candidates.set_active(hosts.index(preferred) if preferred in hosts else 0)
            self.host_changed(self.host)
        self.status.set_text(f"Found {len(hosts)} QNAP NAS device(s). Sign in to see their shared folders." if hosts else
                             "No QNAP responded. Check that it is awake and on this LAN. If it uses a custom web port, enter its IP address manually.")

    def connect_nas(self, _):
        host, username, password = self.host.get_text().strip(), self.username.get_text().strip(), self.password.get_text()
        self.password.set_text("")
        def prepare():
            if self.session:
                self.session.close()
                self.session = None
            self.session = backend.Session(host, username, password)
            return self.session.host_key()
        self.work(prepare, self.trust, "Checking the NAS host identity…")

    def trust(self, fingerprint):
        if fingerprint:
            dialog = Gtk.MessageDialog(transient_for=self, modal=True, message_type=Gtk.MessageType.QUESTION,
                                       buttons=Gtk.ButtonsType.OK_CANCEL, text="Trust this NAS host key?")
            dialog.format_secondary_text("First connection to " + self.session.host + ". Compare this fingerprint with the NAS administrator before continuing:\n\n" + fingerprint)
            accepted = dialog.run() == Gtk.ResponseType.OK
            dialog.destroy()
            if not accepted:
                self.session.close()
                self.session = None
                self.status.set_text("Connection cancelled. No password was sent to the NAS.")
                return
        def connect():
            try:
                self.session.connect()
                return self.session.inventory()
            except Exception:
                self.session.close()
                self.session = None
                raise
        self.work(connect, self.folders_page, "Signing in and checking your accessible shares…")

    def folders_page(self, inventory):
        self.inventory = inventory
        inventory['shares'].sort(key=lambda share: backend.folder_order(share, inventory['managed']))
        old = self.stack.get_child_by_name("folders")
        if old:
            self.stack.remove(old)
            old.destroy()
        box = self.box("folders")
        name = inventory.get('hostname') or self.known_devices.get(self.session.host, {}).get('name') or inventory['model'] or 'QNAP'
        self.label(box, f"{name} — {self.session.host} · {len(inventory['shares'])} shared folder(s) · {len(inventory['managed'])} configured anaNAS folder(s)")
        self.label(box, "Select a folder, then Continue. Expand folders to see their contents. Folders configured with anaNAS are listed first.")
        # name, tag, kind/size, share, relative, directory, loaded, icon, sync icon
        self.folder_model = Gtk.TreeStore(str, str, str, int, str, bool, bool, str, str)
        self.folder_tree = Gtk.TreeView(model=self.folder_model)
        self.folder_tree.set_headers_visible(True)
        for index, name in ((0, 'Shared folder / contents'), (1, 'Sync status'), (2, 'Type / size')):
            renderer = Gtk.CellRendererText()
            column = Gtk.TreeViewColumn(name)
            if index in (0, 1):
                icon = Gtk.CellRendererPixbuf()
                icon.set_property('stock-size', Gtk.IconSize.MENU)
                icon.set_property('xpad', 4)
                column.pack_start(icon, False)
                column.add_attribute(icon, 'icon-name', 7 if index == 0 else 8)
            column.pack_start(renderer, True)
            column.add_attribute(renderer, 'text', index)
            column.set_resizable(True)
            column.set_expand(index == 0)
            self.folder_tree.append_column(column)
        self.folder_tree.connect('row-expanded', self.expand_folder)
        self.folder_tree.connect('row-activated', lambda *_: self.continue_folder(None))
        self.folder_tree.get_selection().connect('changed', self.folder_selected)
        scroll = Gtk.ScrolledWindow()
        scroll.set_policy(Gtk.PolicyType.AUTOMATIC, Gtk.PolicyType.AUTOMATIC)
        scroll.set_min_content_height(280)
        scroll.add(self.folder_tree)
        box.pack_start(scroll, True, True, 0)
        self.selection_label = self.label(box, 'Select a shared folder or one of its subfolders.')
        self.next_button = self.button(box, 'Continue', self.continue_folder)
        self.new_button = self.button(box, 'New folder inside the selected directory…', self.new_folder)
        self.next_button.set_sensitive(False)
        self.new_button.set_sensitive(False)
        self.button(box, 'Back to NAS connection', lambda _: self.stack.set_visible_child_name('connect'))
        for root in backend.browser_roots(inventory):
            tag = backend.sync_tag(root['path'], inventory['managed'])
            parent = self.folder_model.append(None, [root['name'], tag, root['kind'], root['share_index'], root['relative'],
                                                    True, False, 'folder-remote-symbolic', self.pineapple_icon(tag)])
            self.folder_model.append(parent, ['Expand to view contents…', '', '', root['share_index'], '', False, True, '', ''])
        if not inventory['shares']:
            self.label(box, 'No shared folders are accessible to this login. Grant access or create a shared folder in QTS, then reconnect.')
        box.show_all()
        self.stack.set_visible_child_name('folders')
        self.status.set_text('Browsing does not change files or enable synchronization.')
        if inventory['shares']:
            self.folder_tree.get_selection().select_path(Gtk.TreePath.new_from_string('0'))

    def selected_folder(self):
        model, row = self.folder_tree.get_selection().get_selected()
        if row is None or not model[row][5]:
            return None
        share = self.inventory['shares'][model[row][3]]
        relative = model[row][4]
        return share, relative, share['path'] + ('/' + relative if relative else '')

    @staticmethod
    def pineapple_icon(tag):
        return 'ananas-symbolic' if tag.startswith(('Configured with anaNAS', 'Inside an anaNAS')) else ''

    def folder_selected(self, _):
        chosen = self.selected_folder()
        self.next_button.set_sensitive(chosen is not None)
        self.new_button.set_sensitive(chosen is not None)
        self.selection_label.set_text(chosen[2] if chosen else 'Select a folder to continue. Files are shown for preview only.')

    def expand_folder(self, tree, parent, path):
        if self.folder_model[parent][6] or self.busy:
            return
        share_index = self.folder_model[parent][3]
        share = self.inventory['shares'][share_index]
        relative = self.folder_model[parent][4]
        reference = Gtk.TreeRowReference.new(self.folder_model, path)
        def loaded(result):
            row_path = reference.get_path()
            if row_path is None:
                return
            row = self.folder_model.get_iter(row_path)
            expanded = tree.row_expanded(row_path)
            child = self.folder_model.iter_children(row)
            while child is not None:
                self.folder_model.remove(child)
                child = self.folder_model.iter_children(row)
            entries = sorted(result['entries'], key=lambda entry: backend.folder_order(entry, self.inventory['managed']))
            from ananas_control_panel import size
            for entry in entries:
                tag = backend.sync_tag(entry['path'], self.inventory['managed']) if entry['directory'] else ''
                kind = 'Folder' if entry['directory'] else size(entry['bytes']) if entry['kind'] == 'file' else 'Link (not followed)' if entry['kind'] == 'link' else 'Special file'
                icon = 'folder-symbolic' if entry['directory'] else 'emblem-symbolic-link' if entry['kind'] == 'link' else 'text-x-generic-symbolic'
                child = self.folder_model.append(row, [entry['name'], tag, kind, share_index, entry['relative'], entry['directory'], False,
                                                      icon, self.pineapple_icon(tag)])
                if entry['directory']:
                    self.folder_model.append(child, ['Expand to view contents…', '', '', share_index, '', False, True, '', ''])
            if not entries:
                self.folder_model.append(row, ['Empty folder', '', '', share_index, '', False, True, '', ''])
            if result.get('truncated'):
                self.folder_model.append(row, ['Preview limited to 1,000 entries; all contents will be included in sync.', '', '', share_index, '', False, True, '', ''])
            self.folder_model[row][6] = True
            if expanded:
                tree.expand_row(row_path, False)
            self.status.set_text('Select a folder and Continue. Previewing files does not synchronize them.')
        self.work(lambda: self.session.browse(share, relative), loaded, 'Reading folder contents…')

    def continue_folder(self, _):
        chosen = self.selected_folder()
        if chosen:
            self.destination_page(*chosen)

    def new_folder(self, _):
        chosen = self.selected_folder()
        if chosen:
            self.destination_page(*chosen, new_name='anaNAS')

    def destination_page(self, share, relative, path, new_name=''):
        self.chosen_share, self.chosen_relative = share, relative
        self.existing_profile = backend.local_profile(self.session.host, path, self.inventory['shares']) if not new_name else None
        old = self.stack.get_child_by_name('destination')
        if old:
            self.stack.remove(old)
            old.destroy()
        box = self.box('destination')
        self.label(box, 'Selected NAS folder: ' + path)
        self.label(box, backend.sync_tag(path, self.inventory['managed']))
        self.folder = self.field(box, 'Create a new subfolder (optional; leave blank to use the selected folder)', new_name)
        local = self.existing_profile['local']['root'] if self.existing_profile else str(Path.home() / 'QNAP' / (new_name or Path(path).name))
        self.local = self.field(box, 'Save this folder on your PC at', local)
        choose = self.button(box, 'Choose local folder…', self.choose_local)
        advanced = Gtk.Expander(label='Advanced setup options')
        options = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=8)
        advanced.add(options)
        box.pack_start(advanced, False, False, 0)
        self.account = Gtk.ComboBoxText()
        self.label(options, "Run NAS sync as (non-administrator account with write access)")
        for item in self.inventory["accounts"]:
            self.account.append_text(item["name"])
        self.account.set_active(0)
        options.pack_start(self.account, False, False, 0)
        self.save = Gtk.CheckButton(label="Save the SMB login in a root-only file for automatic mounting after reboot")
        self.save.set_active(True)
        options.pack_start(self.save, False, False, 0)
        self.label(box, "Only the selected directory is synchronized, in both directions. Existing setups are preserved.\nAdvanced exclusions and resource limits can be adjusted in the generated profile.\nCurrent engine limits: 64 MiB per file; conflict choices and whole-directory deletion are still in development.")
        self.button(box, "Enable existing sync" if self.existing_profile else "Review and start sync", self.review)
        self.button(box, "Back to shared folders", lambda _: self.stack.set_visible_child_name("folders"))
        if self.existing_profile:
            self.label(box, 'This folder is already configured on this PC. Continuing reuses its existing daemon and certificates; it does not install another helper.')
            self.folder.set_sensitive(False)
            self.local.set_sensitive(False)
            choose.set_sensitive(False)
            advanced.set_sensitive(False)
            self.button(box, 'Open its control panel', lambda _: self.open_panel(self.existing_profile))
        elif not self.inventory["accounts"]:
            self.label(box, "Create a non-administrator QNAP account with write access in QTS, then reconnect. The helper will never run as root.")
        box.show_all()
        self.stack.set_visible_child_name("destination")
        self.status.set_text('Choose a local destination, then review the setup before any files are changed.')

    def choose_local(self, _):
        dialog = Gtk.FileChooserDialog(title="Choose an empty local sync folder", transient_for=self,
                                       action=Gtk.FileChooserAction.SELECT_FOLDER)
        dialog.add_buttons("Cancel", Gtk.ResponseType.CANCEL, "Use this folder", Gtk.ResponseType.OK)
        dialog.set_create_folders(True)
        if dialog.run() == Gtk.ResponseType.OK:
            self.local.set_text(dialog.get_filename())
        dialog.destroy()

    def review(self, _):
        if self.existing_profile:
            self.service(self.existing_profile, True)
            return
        try:
            if not self.save.get_active():
                raise backend.SetupError("Unattended mounting requires the saved SMB login. No credential has been saved yet.")
            if self.account.get_active() < 0:
                raise backend.SetupError("Select an accessible share and a non-admin account first.")
            plan = backend.make_plan(self.session, self.inventory,
                                     self.chosen_share, self.folder.get_text().strip(),
                                     self.local.get_text().strip(), self.inventory["accounts"][self.account.get_active()], relative=self.chosen_relative)
        except backend.SetupError as error:
            self.status.set_text(str(error))
            return
        dialog = Gtk.MessageDialog(transient_for=self, modal=True, message_type=Gtk.MessageType.QUESTION,
                                   buttons=Gtk.ButtonsType.OK_CANCEL, text="Install and start this sync folder?")
        dialog.format_secondary_text(f"NAS: {plan['host']}:{plan['nasRoot']}\nPC: {plan['localRoot']}\n\n"
                                     "The NAS helper and local daemon will start automatically. The SMB login will be stored root-only on this PC. "
                                     "No existing sync instance will be replaced. PC administrator confirmation follows.")
        accepted = dialog.run() == Gtk.ResponseType.OK
        dialog.destroy()
        if accepted:
            self.installing = True
            self.work(lambda: backend.install(self.session, self.inventory, plan,
                      lambda message: GLib.idle_add(self.progress, message)), self.installed, "Preparing installation…")

    def progress(self, message):
        if not self.closed:
            self.status.set_text(message)
        return False

    def installed(self, result):
        self.installing = False
        self.existing_page()
        self.stack.set_visible_child_name("existing")
        self.status.set_text(result["message"] + "\nSettings: " + result["profile"])

    def close_requested(self, *_):
        if self.busy:
            self.status.set_text("Please wait for the current bounded setup step to finish before closing. Closing during installation could leave a partial setup.")
            return True
        self.closed = True
        if self.identity_timer:
            GLib.source_remove(self.identity_timer)
            self.identity_timer = 0
        if self.session:
            self.session.close()
        self.executor.shutdown(wait=False, cancel_futures=True)
        return False


def run():
    app = Gtk.Application(application_id="org.ananas.Setup", flags=Gio.ApplicationFlags.FLAGS_NONE)
    def activate(app):
        if app.get_active_window():
            app.get_active_window().present()
        else:
            Wizard(app)
    app.connect("activate", activate)
    app.run([])


if __name__ == "__main__":
    run()
