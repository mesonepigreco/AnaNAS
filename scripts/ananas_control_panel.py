"""Native, single-instance anaNAS control panel. All I/O runs off the GTK thread."""
import datetime
import hashlib
import queue
import threading
import time

import gi
gi.require_version("Gtk", "3.0")
from gi.repository import Gtk, Gdk, Gio, GLib


def size(value):
    if not isinstance(value, (int, float)) or value < 0:
        return "—"
    for unit in ("B", "KiB", "MiB", "GiB", "TiB"):
        if value < 1024 or unit == "TiB":
            return f"{value:,.1f} {unit}"
        value /= 1024


def local_time(value):
    try:
        return datetime.datetime.fromisoformat(value.replace("Z", "+00:00")).astimezone().strftime("%d %b, %H:%M")
    except (ValueError, AttributeError):
        return "—"


CSS = b"""
window { background: #f6f7f4; color: #24372d; }
headerbar { background: #ffffff; }
.page { padding: 22px; }
.hero { font-size: 26px; font-weight: 700; }
.muted { color: #67766d; }
.card { background: #ffffff; border: 1px solid #dce4dd; border-radius: 12px; padding: 18px; }
.caption { font-weight: 600; color: #52695b; }
.stat { font-size: 22px; font-weight: 700; }
.section { font-size: 18px; font-weight: 700; }
.problem { color: #983a26; }
button { border-radius: 7px; padding: 7px 12px; }
button { background: #ffffff; color: #24372d; border: 1px solid #cbd7cd; }
button label, headerbar label { color: #24372d; }
notebook, notebook stack, notebook header, notebook tab { background: #ffffff; color: #24372d; }
notebook header { border-bottom: 1px solid #dce4dd; }
notebook tab { padding: 8px 18px; }
notebook tab label { color: #24372d; }
notebook tab:checked { border-bottom: 3px solid #438867; }
headerbar button.titlebutton { color: #24372d; }
menu { background-color: #ffffff; color: #24372d; border: 1px solid #cbd7cd; padding: 5px; }
menuitem { background-color: #ffffff; color: #24372d; padding: 8px 14px; border-radius: 5px; }
menuitem label { color: #24372d; }
menuitem:hover { background-color: #dbece0; color: #183d28; }
menuitem:hover label { color: #183d28; }
menuitem:disabled label { color: #89948c; }
button.suggested-action { background: #27724d; color: white; }
progressbar trough { min-height: 8px; border-radius: 5px; background: #e6ece7; }
progressbar progress { background: #438867; border-radius: 5px; }
treeview { background: #ffffff; color: #24372d; }
treeview:selected { background: #dbece0; color: #183d28; }
"""


class Panel(Gtk.Application):
    def __init__(self, client, root):
        app_id = "org.ananas.ControlPanel"
        if getattr(client, "port", 8721) != 8721:
            app_id += ".p" + hashlib.sha256(str(root).encode()).hexdigest()[:16]
        super().__init__(application_id=app_id,
                         flags=Gio.ApplicationFlags.DEFAULT_FLAGS)
        self.client, self.root = client, root
        self.window = None
        self.state = None
        self.connected = False
        self.controlling = False
        self.jobs = queue.Queue(maxsize=8)
        self.stop = threading.Event()
        self.pending_lock = threading.Lock()
        self.pending = None
        self.pending_scheduled = False
        self.generation = 0
        self.age_timer = 0
        self.connect("activate", self.activate_panel)

    def label(self, text="", style=None):
        widget = Gtk.Label(label=text, xalign=0)
        widget.set_selectable(True)
        if style:
            widget.get_style_context().add_class(style)
        return widget

    def activate_panel(self, _app):
        if self.window is None:
            self.build()
            self.hold()
            threading.Thread(target=self.worker, daemon=True).start()
            threading.Thread(target=self.client.stream, args=(self.stop, self.deliver), daemon=True).start()
        self.window.show_all()
        self.window.present()
        if not self.age_timer:
            self.age_timer = GLib.timeout_add_seconds(60, self.refresh_age)
        self.refresh()

    def build(self):
        provider = Gtk.CssProvider()
        provider.load_from_data(CSS)
        Gtk.StyleContext.add_provider_for_screen(Gdk.Screen.get_default(), provider, Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)
        self.window = Gtk.ApplicationWindow(application=self, title="anaNAS · Control panel")
        self.window.set_default_size(1060, 800)
        self.window.set_icon_name("folder-remote")
        self.window.connect("delete-event", self.hide)
        header = Gtk.HeaderBar(title="anaNAS", subtitle="Control panel", show_close_button=True)
        self.window.set_titlebar(header)
        folder = Gtk.Button(label="Open NASdir")
        folder.connect("clicked", self.open_folder)
        header.pack_start(folder)
        self.pause = Gtk.Button(label="Pause sync")
        self.pause.connect("clicked", self.toggle_pause)
        header.pack_end(self.pause)
        self.refresh_button = Gtk.Button(label="Refresh")
        self.refresh_button.connect("clicked", lambda _: self.refresh())
        header.pack_end(self.refresh_button)

        page = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=16)
        page.get_style_context().add_class("page")
        self.window.add(page)
        self.headline = self.label("Connecting to anaNAS…", "hero")
        self.headline.set_ellipsize(3)
        self.headline.set_max_width_chars(65)
        page.pack_start(self.headline, False, False, 0)
        self.location = self.label(str(self.root), "muted")
        page.pack_start(self.location, False, False, 0)
        self.message = self.label("", "problem")
        self.message.set_line_wrap(True)
        page.pack_start(self.message, False, False, 0)

        cards = Gtk.Box(spacing=14, homogeneous=True)
        page.pack_start(cards, False, False, 0)
        self.session, self.session_note = self.transfer_card(cards, "THIS SESSION")
        self.daily, self.daily_note = self.transfer_card(cards, "LAST 24 HOURS")
        nas = self.card(cards, "NAS STORAGE")
        self.free = self.label("Loading…", "stat")
        nas.pack_start(self.free, False, False, 0)
        self.capacity_bar = Gtk.ProgressBar()
        nas.pack_start(self.capacity_bar, False, False, 0)
        self.capacity_note = self.label("", "muted")
        self.capacity_note.set_line_wrap(True)
        nas.pack_start(self.capacity_note, False, False, 0)

        notebook = Gtk.Notebook()
        page.pack_start(notebook, True, True, 0)
        folders = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=10, margin=14)
        notebook.append_page(folders, Gtk.Label(label="Folder sizes"))
        self.tree_summary = self.label("Loading indexed folder sizes…", "section")
        folder_header = Gtk.Box(spacing=12)
        folder_header.pack_start(self.tree_summary, True, True, 0)
        self.show_hidden = Gtk.CheckButton(label="Show hidden folders")
        self.show_hidden.connect("toggled", lambda _: self.refresh())
        folder_header.pack_end(self.show_hidden, False, False, 0)
        folders.pack_start(folder_header, False, False, 0)
        note = self.label("Expand to explore · Double-click to open · Right-click for folder and terminal actions", "muted")
        folders.pack_start(note, False, False, 0)
        self.model = Gtk.TreeStore(str, str, str, str, int, str, bool)
        self.tree = Gtk.TreeView(model=self.model)
        self.tree.set_enable_tree_lines(True)
        self.tree.set_tooltip_column(5)
        for column, title in enumerate(("Folder", "On this computer", "Last synced to NAS", "Files")):
            cell = Gtk.CellRendererText()
            if column == 0:
                cell.set_property("ellipsize", 3)
            view_column = Gtk.TreeViewColumn(title, cell, text=column)
            view_column.set_resizable(True)
            view_column.set_expand(column == 0)
            if column == 0:
                view_column.set_min_width(250)
            self.tree.append_column(view_column)
        self.tree.append_column(Gtk.TreeViewColumn("Share of parent", Gtk.CellRendererProgress(), value=4))
        self.tree.connect("row-expanded", self.expand)
        self.tree.connect("row-activated", self.activate_folder)
        self.tree.connect("button-press-event", self.folder_button_press)
        self.tree.connect("popup-menu", self.folder_keyboard_menu)
        scroll = Gtk.ScrolledWindow()
        scroll.set_policy(Gtk.PolicyType.AUTOMATIC, Gtk.PolicyType.AUTOMATIC)
        scroll.add(self.tree)
        folders.pack_start(scroll, True, True, 0)
        foot = self.label("Totals include hidden files. Logical sizes come from the index; NAS volume usage includes other shares, history and the recycle bin.", "muted")
        foot.set_line_wrap(True)
        folders.pack_start(foot, False, False, 0)

        recent = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=10, margin=14)
        notebook.append_page(recent, Gtk.Label(label="Recent activity"))
        self.recent_title = self.label("Recent completed updates", "section")
        recent.pack_start(self.recent_title, False, False, 0)
        self.recent_model = Gtk.ListStore(str, str, str)
        recent_view = Gtk.TreeView(model=self.recent_model)
        for i, title in enumerate(("When", "Change", "File or folder")):
            col = Gtk.TreeViewColumn(title, Gtk.CellRendererText(), text=i)
            col.set_expand(i == 2)
            col.set_resizable(True)
            recent_view.append_column(col)
        recent_scroll = Gtk.ScrolledWindow()
        recent_scroll.add(recent_view)
        recent.pack_start(recent_scroll, True, True, 0)
        self.footer = self.label("Only on your NAS network · Transfers include encrypted protocol traffic", "muted")
        page.pack_start(self.footer, False, False, 0)

    def card(self, row, title):
        card = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=10)
        card.get_style_context().add_class("card")
        card.pack_start(self.label(title, "caption"), False, False, 0)
        row.pack_start(card, True, True, 0)
        return card

    def transfer_card(self, row, title):
        card = self.card(row, title)
        value = self.label("↑ —    ↓ —", "stat")
        card.pack_start(value, False, False, 0)
        note = self.label("Uploaded / downloaded", "muted")
        note.set_line_wrap(True)
        card.pack_start(note, False, False, 0)
        return value, note

    def hide(self, *_):
        self.window.hide()
        if self.age_timer:
            GLib.source_remove(self.age_timer)
            self.age_timer = 0
        return True

    def refresh_age(self):
        self.submit(self.client.status, lambda state, error: self.render(state, error))
        return True

    def submit(self, work, done):
        try:
            self.jobs.put_nowait((work, done))
            return True
        except queue.Full:
            self.message.set_text("Please wait for the current request, then try again.")
            return False

    def worker(self):
        while not self.stop.is_set():
            work, done = self.jobs.get()
            value, error = None, None
            try:
                value = work()
            except Exception as exc:
                print(f"Control panel request: {exc}", flush=True)
                error = "Could not load information from anaNAS. Check the connection and refresh."
            GLib.idle_add(done, value, error)

    def deliver(self, state, error):
        with self.pending_lock:
            self.pending = (state, error)
            if not self.pending_scheduled:
                self.pending_scheduled = True
                GLib.idle_add(self.apply_delivery)

    def apply_delivery(self):
        with self.pending_lock:
            state, error = self.pending
            self.pending_scheduled = False
        self.render(state, error)
        return False

    def render(self, state, error):
        was_connected = self.connected
        self.connected = error is None and state is not None
        if not self.connected:
            self.headline.set_text("Waiting for the background service")
            self.message.set_text("anaNAS is reconnecting automatically. Previously displayed figures may be out of date.")
            self.pause.set_sensitive(False)
            return False
        self.state = state
        if not was_connected:
            self.message.set_text("")
        self.headline.set_text("Synchronization paused" if state.get("paused") else state.get("syncReason", "Connected"))
        self.pause.set_label("Resume sync" if state.get("paused") else "Pause sync")
        self.pause.set_sensitive(not self.controlling)
        traffic = state.get("traffic", {})
        session = traffic.get("session", {"upload": state.get("uploadBytes"), "download": state.get("downloadBytes")})
        daily = traffic.get("last24Hours", {})
        self.session.set_text(f"↑ {size(session.get('upload'))}   ↓ {size(session.get('download'))}")
        self.daily.set_text(f"↑ {size(daily.get('upload'))}   ↓ {size(daily.get('download'))}")
        self.session_note.set_text("Uploaded / downloaded\nStarted " + local_time(traffic.get("startedAt")))
        self.daily_note.set_text("Uploaded / downloaded\nRecording since " + local_time(traffic.get("recordingSince")))
        if traffic.get("historyError"):
            self.daily_note.set_text("Traffic history could not be saved. Figures may be incomplete after a restart.")
        self.recent_title.set_text(f"{state.get('updatedLast24Hours', 0):,} completed updates in the last 24 hours")
        self.recent_model.clear()
        for item in state.get("recentUpdates") or []:
            self.recent_model.append([local_time(item.get("at")),
                                      f"{item.get('direction', '')} · {item.get('action', '')}", item.get("path", "")])
        return False

    def toggle_pause(self, _button):
        if not self.connected or self.controlling:
            return
        paused = not self.state.get("paused")
        self.controlling = True
        self.pause.set_sensitive(False)
        def done(_value, error):
            self.controlling = False
            self.pause.set_sensitive(self.connected)
            self.message.set_text("Could not save the pause setting. Please try again." if error else "")
            if not error:
                self.submit(self.client.status, self.render)
        if not self.submit(lambda: self.client.pause(paused), done):
            self.controlling = False
            self.pause.set_sensitive(True)

    def refresh(self):
        self.message.set_text("")
        self.submit(self.client.status, self.render)
        self.refresh_button.set_sensitive(False)
        self.submit(lambda: self.client.storage(), self.storage_done)

    def storage_done(self, data, error):
        self.refresh_button.set_sensitive(True)
        if error:
            self.message.set_text(error)
            return False
        cap = data["capacity"]
        if cap.get("available"):
            self.free.set_text(size(cap["free"]) + " available")
            self.capacity_note.set_text(f"{size(cap['used'])} used of {size(cap['total'])}\nChecked {local_time(cap['checkedAt'])}")
            self.capacity_bar.set_fraction(min(1, cap["used"] / max(1, cap["total"])))
        else:
            self.free.set_text("NAS unavailable")
            self.capacity_note.set_text(cap.get("message", "Connect to the NAS network and refresh."))
            self.capacity_bar.set_fraction(0)
        self.generation += 1
        self.model.clear()
        usage = data["usage"]
        self.tree_summary.set_text(f"{size(usage['localBytes'])} on this computer · {usage['files']:,} files")
        root = self.model.append(None, self.row(usage, usage["localBytes"], True))
        self.populate(root, usage)
        self.tree.expand_row(self.model.get_path(root), False)
        return False

    def row(self, item, total, loaded=False):
        return [item["name"], size(item["localBytes"]), size(item["syncedBytes"]),
                f"{item['files']:,}", min(100, round(item["localBytes"] * 100 / max(1, total))), item["path"], loaded]

    def populate(self, parent, usage):
        # Removing the last loading child makes GTK collapse the parent. Keep
        # the user's current choice, including a collapse while I/O was pending.
        parent_path = self.model.get_path(parent)
        was_expanded = self.tree.row_expanded(parent_path)
        child = self.model.iter_children(parent)
        while child:
            self.model.remove(child)
            child = self.model.iter_children(parent)
        for item in usage["children"]:
            if item["name"].startswith(".") and not self.show_hidden.get_active():
                continue
            node = self.model.append(parent, self.row(item, usage["localBytes"]))
            self.model.append(node, ["Expand to load…", "", "", "", 0, "", True])
        if usage.get("truncated"):
            self.message.set_text("Showing the 1,000 largest subfolders. Open a subfolder to explore further.")
        if was_expanded:
            self.tree.expand_row(parent_path, False)

    def expand(self, _tree, node, path):
        if self.model[node][6]:
            return
        self.model[node][6] = True
        reference = Gtk.TreeRowReference.new(self.model, path)
        generation = self.generation
        folder = self.model[node][5]
        def done(data, error):
            if generation != self.generation or not reference.valid():
                return False
            current = self.model.get_iter(reference.get_path())
            if error:
                self.model[current][6] = False
                self.message.set_text("Could not load this folder. Collapse it and expand it to try again.")
            else:
                self.populate(current, data["usage"])
            return False
        if not self.submit(lambda: self.client.storage(folder), done):
            self.model[node][6] = False

    def open_folder(self, _button):
        self.open_directory("")

    def activate_folder(self, _tree, path, _column):
        folder = self.folder_at(path)
        if folder is None:
            return
        self.open_directory(folder)

    def folder_at(self, path):
        folder = self.model[path][5]
        # Loading placeholders are not directories.
        return None if not folder and path.get_depth() > 1 else folder

    def folder_button_press(self, tree, event):
        if event.button != 3:
            return False
        hit = tree.get_path_at_pos(int(event.x), int(event.y))
        if hit is None:
            return False
        tree.set_cursor(hit[0])
        return self.show_folder_menu(hit[0], event)

    def folder_keyboard_menu(self, tree):
        model, selected = tree.get_selection().get_selected()
        if selected is None:
            return False
        return self.show_folder_menu(model.get_path(selected))

    def show_folder_menu(self, path, event=None):
        folder = self.folder_at(path)
        if folder is None:
            return False
        if getattr(self, "folder_menu", None) is not None:
            self.folder_menu.destroy()
        self.folder_menu = Gtk.Menu()
        self.folder_menu.attach_to_widget(self.tree, None)
        for title, action in (("Open directory", self.open_directory),
                              ("Open terminal here", self.open_terminal)):
            item = Gtk.MenuItem(label=title)
            item.connect("activate", lambda _item, action=action: action(folder))
            self.folder_menu.append(item)
        self.folder_menu.show_all()
        if event is not None:
            self.folder_menu.popup_at_pointer(event)
        else:
            rectangle = self.tree.get_cell_area(path, self.tree.get_column(0))
            self.folder_menu.popup_at_rect(self.tree.get_bin_window(), rectangle,
                                          Gdk.Gravity.SOUTH_WEST, Gdk.Gravity.NORTH_WEST, None)
        return True

    def open_terminal(self, relative, software=False):
        from pathlib import PurePosixPath
        path = PurePosixPath(relative)
        if path.is_absolute() or ".." in path.parts:
            self.message.set_text("This folder path is invalid. Refresh the folder list and try again.")
            return
        directory = self.root.joinpath(relative)
        try:
            # Respect the desktop's default terminal. The selected directory is
            # one literal argument, never shell code; the terminal has its own
            # service so it cannot exhaust the control panel's memory budget.
            command = [
                "/usr/bin/systemd-run", "--user", "--collect", "--quiet", "--wait",
                "--expand-environment=no", "--property=Type=exec",
            ]
            if software:
                command += ["/usr/bin/env", "GSK_RENDERER=cairo"]
            command += ["/usr/bin/xdg-terminal-exec", "--dir=" + str(directory)]
            started = time.monotonic()
            process = Gio.Subprocess.new(command, Gio.SubprocessFlags.NONE)
            process.wait_check_async(None, lambda source, result: self.terminal_done(source, result, directory, software, started))
        except GLib.Error as error:
            self.terminal_error(directory, error)

    def terminal_done(self, process, result, directory, software, started):
        try:
            process.wait_check_finish(result)
        except GLib.Error as error:
            if not software and time.monotonic() - started < 10:
                # GTK terminals can encounter the same post-update GPU mismatch
                # as Nautilus. Retry one failed startup, never a later shell exit.
                print(f"Terminal startup failed: {error}; retrying with software rendering", flush=True)
                self.open_terminal(str(directory.relative_to(self.root)), software=True)
            else:
                self.terminal_error(directory, error)

    def terminal_error(self, directory, error):
        print(f"Terminal launch for {directory}: {error}", flush=True)
        self.message.set_text(f"Could not open a terminal in {directory.name}. Check that a default terminal and xdg-terminal-exec are installed, then try again.")

    def open_directory(self, relative):
        from pathlib import PurePosixPath
        path = PurePosixPath(relative)
        if path.is_absolute() or ".." in path.parts:
            self.message.set_text("This folder path is invalid. Refresh the folder list and try again.")
            return
        directory = self.root.joinpath(relative)
        try:
            Gio.AppInfo.launch_default_for_uri_async(directory.as_uri(), None, None,
                lambda _source, result: self.directory_opened(result, directory))
        except (GLib.Error, ValueError) as error:
            self.directory_error(directory, error)

    def directory_opened(self, result, directory):
        try:
            Gio.AppInfo.launch_default_for_uri_finish(result)
        except GLib.Error as error:
            app = Gio.AppInfo.get_default_for_type("inode/directory", False)
            if (app is not None and app.get_id() == "org.gnome.Nautilus.desktop"
                    and error.matches(Gio.dbus_error_quark(), Gio.DBusError.NO_REPLY)):
                print(f"Folder launch failed: {error}; retrying Nautilus with software rendering", flush=True)
                try:
                    process = Gio.Subprocess.new([
                        "/usr/bin/systemd-run", "--user", "--collect", "--quiet",
                        "--expand-environment=no",
                        "--property=Type=exec", "/usr/bin/env", "GSK_RENDERER=cairo",
                        "/usr/bin/nautilus", "--new-window", directory.as_uri(),
                    ], Gio.SubprocessFlags.NONE)
                    process.wait_check_async(None, lambda source, result: self.directory_fallback_done(source, result, directory))
                    return
                except GLib.Error as fallback_error:
                    error = fallback_error
            self.directory_error(directory, error)
        else:
            self.message.set_text("")

    def directory_fallback_done(self, process, result, directory):
        try:
            process.wait_check_finish(result)
        except GLib.Error as error:
            self.directory_error(directory, error)
        else:
            self.message.set_text("")

    def directory_error(self, directory, error):
        print(f"Could not open {directory}: {error}", flush=True)
        self.message.set_text(f"The file manager could not open {directory.name}. Try again, or open {directory} directly in Files.")


def run(client, root):
    Panel(client, root).run([])
