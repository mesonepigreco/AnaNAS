#!/usr/bin/python3
"""Session-bus panel controls; only literal-loopback HTTP, never NAS I/O.

Requires python3-gi and a StatusNotifierItem host (Ubuntu AppIndicators/KDE).
Two bounded workers: one event stream and one serialized control request.
"""
import argparse
import datetime
import html.parser
import http.client
import json
import os
from pathlib import Path
import queue
import signal
import threading
import urllib.parse

from gi.repository import Gio, GLib, GLibUnix


ITEM = "org.kde.StatusNotifierItem"
MENU = "com.canonical.dbusmenu"
BUS_NAME = "org.kde.StatusNotifierItem.nas_sync"
MAX_RESPONSE = 65536
RECENT_SLOTS = 12


def user_error(action, error):
    detail = str(error).strip() or "unknown error"
    return f"{action}: {detail}"


class TokenParser(html.parser.HTMLParser):
    def __init__(self):
        super().__init__()
        self.token = None

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        if tag == "meta" and attrs.get("name") == "nas-sync-csrf":
            value = attrs.get("content", "")
            if len(value) == 64 and all(c in "0123456789abcdef" for c in value):
                self.token = value


class Client:
    def __init__(self, port):
        if isinstance(port, bool) or not isinstance(port, int) or not 1 <= port <= 65535:
            raise ValueError("The daemon requires a nonzero webPort")
        self.port = port
        self.url = f"http://127.0.0.1:{port}/"

    def connection(self):
        # HTTPConnection bypasses environment proxy settings and follows no redirects.
        return http.client.HTTPConnection("127.0.0.1", self.port, timeout=5)

    def token(self):
        conn = self.connection()
        try:
            conn.request("GET", "/")
            response = conn.getresponse()
            data = response.read(MAX_RESPONSE + 1)
            if response.status != 200 or len(data) > MAX_RESPONSE:
                raise ValueError("Daemon control page unavailable")
            parser = TokenParser()
            parser.feed(data.decode("utf-8"))
            if parser.token is None:
                raise ValueError("Daemon control token unavailable")
            return parser.token
        finally:
            conn.close()

    def pause(self, paused):
        token = self.token()
        conn = self.connection()
        try:
            conn.request("POST", "/api/pause", json.dumps({"paused": paused}), {
                "Content-Type": "application/json", "X-Nas-Sync-CSRF": token,
            })
            response = conn.getresponse()
            if response.status != 204:
                raise ValueError(f"Control failed (HTTP {response.status})")
        finally:
            conn.close()

    def status(self):
        conn = self.connection()
        try:
            conn.request("GET", "/api/status")
            response = conn.getresponse()
            data = response.read(MAX_RESPONSE + 1)
            if response.status != 200 or len(data) > MAX_RESPONSE:
                raise ValueError("Daemon status unavailable")
            state = json.loads(data)
            if not isinstance(state, dict) or not isinstance(state.get("paused"), bool):
                raise ValueError("Invalid daemon status")
            return state
        finally:
            conn.close()

    def pending_files(self, after=""):
        return self.pending_request("GET", "/api/pending?" + urllib.parse.urlencode({"after": after}))

    def confirm_sync(self, path=None, generation=None, all_files=False):
        value = {"all": True} if all_files else {"path": path, "generation": generation}
        return self.pending_request("POST", "/api/confirm-sync", value)

    def pending_request(self, method, path, value=None):
        token = self.token()
        conn = self.connection()
        try:
            conn.request(method, path, None if value is None else json.dumps(value),
                         {"X-Nas-Sync-CSRF": token, "Content-Type": "application/json"})
            response = conn.getresponse()
            data = response.read(2 * 1024 * 1024 + 1)
            if response.status != 200 or len(data) > 2 * 1024 * 1024:
                raise ValueError("Pending sync request failed; refresh and try again")
            return json.loads(data)
        finally:
            conn.close()

    def storage(self, path=""):
        token = self.token()
        conn = self.connection()
        try:
            conn.request("GET", "/api/storage?" + urllib.parse.urlencode({"path": path}),
                         headers={"X-Nas-Sync-CSRF": token})
            response = conn.getresponse()
            data = response.read(2 * 1024 * 1024 + 1)
            if response.status != 200 or len(data) > 2 * 1024 * 1024:
                raise ValueError("Folder sizes could not be loaded. Please refresh.")
            return json.loads(data)
        finally:
            conn.close()

    def stream(self, stop, deliver):
        delay = 1
        while not stop.is_set():
            conn = self.connection()
            try:
                token = self.token()
                conn.request("GET", "/api/events", headers={"X-Nas-Sync-CSRF": token})
                response = conn.getresponse()
                if response.status != 200:
                    raise ValueError(f"Status stream unavailable (HTTP {response.status})")
                # A quiet daemon emits nothing. There is no idle timeout/polling.
                conn.sock.settimeout(None)
                while not stop.is_set():
                    line = response.readline(MAX_RESPONSE + 1)
                    if not line or len(line) > MAX_RESPONSE or not line.endswith(b"\n"):
                        raise ValueError("Daemon status stream closed or invalid")
                    state = json.loads(line)
                    if not isinstance(state, dict) or not isinstance(state.get("paused"), bool):
                        raise ValueError("Invalid daemon status")
                    delay = 1
                    deliver(state, None)
            except (OSError, ValueError, http.client.HTTPException) as error:
                deliver(None, user_error(
                    "Cannot contact the anaNAS background service", error))
            finally:
                conn.close()
            stop.wait(delay)
            delay = min(delay * 2, 30)


def interface_xml():
    props = {"Category": "s", "Id": "s", "Title": "s", "Status": "s",
             "WindowId": "i", "IconName": "s", "IconThemePath": "s",
             "IconPixmap": "a(iiay)", "OverlayIconName": "s", "OverlayIconPixmap": "a(iiay)",
             "AttentionIconName": "s", "AttentionIconPixmap": "a(iiay)",
             "AttentionMovieName": "s", "Menu": "o", "ItemIsMenu": "b",
             "XAyatanaLabel": "s", "XAyatanaLabelGuide": "s"}
    fields = "".join(f'<property name="{k}" type="{v}" access="read"/>' for k, v in props.items())
    actions = "".join(f'<method name="{name}"><arg type="i" direction="in"/><arg type="i" direction="in"/></method>'
                      for name in ("Activate", "SecondaryActivate", "ContextMenu"))
    return f'''<node><interface name="{ITEM}">{fields}{actions}
      <method name="Scroll"><arg type="i" direction="in"/><arg type="s" direction="in"/></method>
      <signal name="NewIcon"/><signal name="NewOverlayIcon"/><signal name="NewTitle"/>
      <signal name="NewStatus"><arg type="s"/></signal></interface>
      <interface name="{MENU}">
      <property name="Version" type="u" access="read"/>
      <property name="TextDirection" type="s" access="read"/>
      <property name="Status" type="s" access="read"/>
      <property name="IconThemePath" type="as" access="read"/>
      <method name="GetLayout"><arg type="i" direction="in"/><arg type="i" direction="in"/>
        <arg type="as" direction="in"/><arg type="u" direction="out"/><arg type="(ia{{sv}}av)" direction="out"/></method>
      <method name="GetGroupProperties"><arg type="ai" direction="in"/><arg type="as" direction="in"/>
        <arg type="a(ia{{sv}})" direction="out"/></method>
      <method name="GetProperty"><arg type="i" direction="in"/><arg type="s" direction="in"/><arg type="v" direction="out"/></method>
      <method name="Event"><arg type="i" direction="in"/><arg type="s" direction="in"/>
        <arg type="v" direction="in"/><arg type="u" direction="in"/></method>
      <method name="EventGroup"><arg type="a(isvu)" direction="in"/><arg type="ai" direction="out"/></method>
      <method name="AboutToShow"><arg type="i" direction="in"/><arg type="b" direction="out"/></method>
      <method name="AboutToShowGroup"><arg type="ai" direction="in"/><arg type="ai" direction="out"/><arg type="ai" direction="out"/></method>
      <signal name="LayoutUpdated"><arg type="u"/><arg type="i"/></signal>
      <signal name="ItemsPropertiesUpdated"><arg type="a(ia{{sv}})"/><arg type="a(ias)"/></signal>
      </interface></node>'''


def size(value):
    if not isinstance(value, (int, float)) or value < 0:
        return "unknown"
    for unit in ("B", "KiB", "MiB", "GiB", "TiB"):
        if value < 1024:
            return f"{value:.1f} {unit}"
        value /= 1024
    return f"{value:.1f} PiB"


class Indicator:
    def __init__(self, client, root, configured=True):
        self.client, self.root = client, root
        self.configured = configured
        self.state = None
        self.error = "Connecting to daemon…"
        self.action_error = None
        self.bus = None
        self.revision = 0
        self.busy = False
        # The menu structure is fixed and is already available to the first
        # GetLayout call.  Treat the initial connecting menu as the baseline:
        # emitting LayoutUpdated while GNOME is doing its first asynchronous
        # layout/property load can cancel that load and leave blank cached rows.
        self.last_menu = tuple(self.items().items())
        self.last_icon = None
        self.last_overlay = None
        self.last_title = None
        self.stop = threading.Event()
        self.commands = queue.Queue(maxsize=1)
        self.delivery_lock = threading.Lock()
        self.pending = None
        self.delivery_scheduled = False
        self.loop = GLib.MainLoop()
        self.name_id = Gio.bus_own_name(Gio.BusType.SESSION, BUS_NAME, Gio.BusNameOwnerFlags.NONE,
                                       self.acquired, None, self.lost)
        self.watch_id = 0

    def acquired(self, conn, _name):
        self.bus = conn
        info = Gio.DBusNodeInfo.new_for_xml(interface_xml())
        self.item_id = conn.register_object("/StatusNotifierItem", info.interfaces[0], self.call, self.prop, None)
        self.menu_id = conn.register_object("/Menu", info.interfaces[1], self.call, self.prop, None)
        self.watch_id = Gio.bus_watch_name_on_connection(conn, "org.kde.StatusNotifierWatcher",
            Gio.BusNameWatcherFlags.NONE, self.watcher_ready, None)
        if self.configured:
            threading.Thread(target=self.client.stream, args=(self.stop, self.deliver), daemon=True).start()
        threading.Thread(target=self.control_worker, daemon=True).start()

    def lost(self, _conn, _name):
        self.quit()

    def watcher_ready(self, conn, _name, _owner):
        def finished(connection, result):
            try:
                connection.call_finish(result)
            except GLib.Error as error:
                print(f"Panel registration failed: {error}", flush=True)
        conn.call("org.kde.StatusNotifierWatcher", "/StatusNotifierWatcher",
                  "org.kde.StatusNotifierWatcher", "RegisterStatusNotifierItem",
                  GLib.Variant("(s)", ("/StatusNotifierItem",)), None,
                  Gio.DBusCallFlags.NONE, 5000, None, finished)

    def deliver(self, state, error):
        # Stream worker can schedule at most one outstanding UI callback.
        with self.delivery_lock:
            self.pending = (state, error)
            if not self.delivery_scheduled:
                self.delivery_scheduled = True
                GLib.idle_add(self.apply_delivery)

    def apply_delivery(self):
        with self.delivery_lock:
            self.state, self.error = self.pending
            self.delivery_scheduled = False
        self.changed()
        return False

    def changed(self):
        if self.bus is None:
            return
        # Metadata counters can change frequently without changing the panel.
        # Avoid making the shell reload/repaint an unchanged menu or icon.
        menu = tuple(self.items().items())
        if menu != self.last_menu:
            old = dict(self.last_menu or ())
            self.last_menu = menu
            if old.keys() != dict(menu).keys():
                self.revision = (self.revision + 1) % (2**32)
                self.bus.emit_signal(None, "/Menu", MENU, "LayoutUpdated", GLib.Variant("(ui)", (self.revision, 0)))
            else:
                # GNOME requests only type/children-display after LayoutUpdated;
                # existing labels are cached until ItemsPropertiesUpdated.
                changed = [(item_id, self.properties(item_id, []))
                           for item_id, values in menu if old.get(item_id) != values]
                self.bus.emit_signal(None, "/Menu", MENU, "ItemsPropertiesUpdated",
                                     GLib.Variant("(a(ia{sv})a(ias))", (changed, [])))
        icon = self.icon()
        if icon != self.last_icon:
            self.last_icon = icon
            self.bus.emit_signal(None, "/StatusNotifierItem", ITEM, "NewIcon", None)
        overlay = self.overlay()
        if overlay != self.last_overlay:
            self.last_overlay = overlay
            self.bus.emit_signal(None, "/StatusNotifierItem", ITEM, "NewOverlayIcon", None)
        title = self.summary()
        if title != self.last_title:
            self.last_title = title
            self.bus.emit_signal(None, "/StatusNotifierItem", ITEM, "NewTitle", None)

    @staticmethod
    def menu_text(value, limit=72):
        # DBusMenu labels do not wrap in GNOME. Bound every label, including
        # recent paths, and keep embedded newlines out of menu rows.
        text = " ".join(str(value).split())
        return text if len(text) <= limit else text[:limit - 1] + "…"

    def sync_summary(self):
        reason = self.state.get("syncReason", "running")
        if reason.startswith("Eligible files synced; some paths need attention"):
            return "Synced eligible files · some paths need attention"
        if reason.startswith("Synchronization needs attention"):
            return "Synchronization needs attention"
        return self.menu_text(reason, 60)

    def summary(self):
        if not self.configured:
            return "anaNAS — setup required"
        if self.error:
            return "anaNAS — daemon unavailable"
        if self.state.get("paused"):
            return "anaNAS — paused"
        if not self.state.get("automaticWrites"):
            return "anaNAS — transfers disabled"
        return "anaNAS — " + self.sync_summary()

    def icon(self):
        return "ananas-symbolic"

    def overlay(self):
        if self.error:
            return "network-offline-symbolic"
        if self.state.get("paused"):
            return "media-playback-pause-symbolic"
        if not self.state.get("automaticWrites"):
            return "dialog-warning-symbolic"
        return ""

    def prop(self, _conn, _sender, _path, iface, name):
        if iface == MENU:
            return {"Version": GLib.Variant("u", 3), "TextDirection": GLib.Variant("s", "ltr"),
                    "Status": GLib.Variant("s", "normal"), "IconThemePath": GLib.Variant("as", [])}.get(name)
        icon_dir = Path(os.environ.get("XDG_DATA_HOME", Path.home() / ".local/share")) / "anaNAS/icons"
        values = {"Category": "ApplicationStatus", "Id": "anaNAS", "Title": self.summary(),
                  "Status": "Active", "IconName": self.icon(), "IconThemePath": str(icon_dir),
                  "OverlayIconName": self.overlay(), "AttentionIconName": "dialog-warning-symbolic", "AttentionMovieName": "",
                  "XAyatanaLabel": "anaNAS", "XAyatanaLabelGuide": "anaNAS"}
        if name in values:
            return GLib.Variant("s", values[name])
        if name.endswith("Pixmap"):
            return GLib.Variant("a(iiay)", [])
        return {"WindowId": GLib.Variant("i", 0), "Menu": GLib.Variant("o", "/Menu"),
                "ItemIsMenu": GLib.Variant("b", True)}.get(name)

    def items(self):
        state = self.state or {}
        traffic = "Traffic unavailable" if self.error else (
            f"Since daemon start: ↑ {size(state.get('uploadBytes'))}  ↓ {size(state.get('downloadBytes'))}")
        problem = self.error or self.action_error
        reason = f"Problem: {problem}" if problem else state.get("syncReason", "")
        if not problem and reason.startswith("Eligible files synced; some paths need attention"):
            reason = "Open Control panel → Pending files for details"
        elif not problem and len(reason) > 72:
            reason = "Open Control panel for full status details"
        if not self.configured:
            reason = "Choose Set up QNAP to connect your NAS and select a local folder."
        count = state.get("updatedLast24Hours")
        count_label = "unavailable" if self.error or not isinstance(count, int) or count < 0 else str(count)
        menu = {1: (self.summary(), False), 2: (traffic, False), 3: (reason, False),
                8: (f"Recent updates — {count_label} in the last 24 hours", True)}
        recent = state.get("recentUpdates") if isinstance(state.get("recentUpdates"), list) else []
        for offset in range(RECENT_SLOTS):
            label = self.recent_label(recent[offset]) if offset < len(recent) else (
                "No completed updates recorded yet" if offset == 0 else "No additional recent update")
            menu[9 + offset] = (label, False)
        menu.update({
            4: ("Saving…" if self.busy else "Resume sync" if state.get("paused") else "Pause sync",
                not self.busy and self.error is None),
            5: ("Open NASdir", self.configured), 6: ("Control panel…", True),
            21: ("Set up QNAP…" if self.error or not state.get("automaticWrites") else "Manage sync folders…", True),
            7: ("Refresh daemon status", self.configured and not self.busy),
        })
        return {key: (self.menu_text(label), enabled) for key, (label, enabled) in menu.items()}

    @staticmethod
    def recent_label(update):
        if not isinstance(update, dict):
            return "Invalid recent update"
        path = update.get("path")
        if not isinstance(path, str) or not path:
            path = "unknown path"
        direction = "↑" if update.get("direction") == "to NAS" else "↓"
        action = update.get("action") if update.get("action") in ("added", "updated", "deleted", "folder") else "updated"
        stamp = update.get("at")
        when = ""
        if isinstance(stamp, str):
            try:
                parsed = datetime.datetime.fromisoformat(stamp.replace("Z", "+00:00"))
                when = " — " + parsed.astimezone().strftime("%H:%M")
            except ValueError:
                pass
        return f"  {direction} {path} ({action}){when}"

    def properties(self, item_id, names):
        if item_id == 0:
            props = {"children-display": GLib.Variant("s", "submenu")}
        else:
            # GNOME can request every property separately while opening the
            # menu. Use the already-published snapshot instead of rebuilding
            # the full menu for every synchronous D-Bus call.
            label, enabled = dict(self.last_menu)[item_id]
            props = {"label": GLib.Variant("s", self.menu_text(label)), "enabled": GLib.Variant("b", enabled),
                     "visible": GLib.Variant("b", self.item_visible(item_id))}
            if item_id == 8:
                props["children-display"] = GLib.Variant("s", "submenu")
        return {k: v for k, v in props.items() if not names or k in names}

    def item_visible(self, item_id):
        if not 9 <= item_id < 9 + RECENT_SLOTS:
            return True
        recent = (self.state or {}).get("recentUpdates")
        if not isinstance(recent, list) or not recent:
            return item_id == 9
        return item_id - 9 < len(recent)

    def event(self, item_id, event):
        if event != "clicked":
            return
        if not self.configured:
            if item_id == 6:
                item_id = 21
            elif item_id in (4, 5, 7):
                return
        if item_id == 4 and not self.busy and self.error is None:
            self.busy = True
            self.commands.put_nowait(not self.state["paused"])
            self.changed()
        elif item_id == 21:
            try:
                process = Gio.Subprocess.new([
                    "/usr/bin/systemd-run", "--user", "--collect", "--quiet",
                    "--expand-environment=no", "--property=Type=exec",
                    "--property=MemoryMax=512M", "--property=CPUQuota=100%",
                    "/usr/bin/python3", str(Path(__file__).resolve()), "--setup",
                ], Gio.SubprocessFlags.NONE)
                process.wait_check_async(None, self.setup_done)
            except GLib.Error:
                self.action_done("Could not start QNAP setup. Run the anaNAS installer to repair the setup components.")
        elif item_id == 6:
            try:
                process = Gio.Subprocess.new([
                    "/usr/bin/systemd-run", "--user", "--collect", "--quiet",
                    "--expand-environment=no",
                    "--property=Type=exec", "--property=MemoryHigh=128M",
                    "--property=MemoryMax=192M", "--property=CPUQuota=50%",
                    "/usr/bin/python3", str(Path(__file__).resolve()),
                    "--control-panel", "--port", str(self.client.port), "--root", str(self.root),
                ], Gio.SubprocessFlags.NONE)
                process.wait_check_async(None, self.panel_done)
            except GLib.Error as error:
                self.panel_failed(error)
        elif item_id == 5:
            uri = self.root.as_uri() if item_id == 5 else self.client.url
            action = "Could not open NASdir" if item_id == 5 else "Could not open anaNAS status"
            try:
                Gio.AppInfo.launch_default_for_uri_async(
                    uri, None, None,
                    lambda source, result: self.open_done(source, result, action, item_id == 5))
            except GLib.Error as error:
                self.report_open_error(action, error, item_id == 5)
        elif item_id == 7 and not self.busy:
            self.busy = True
            self.commands.put_nowait("refresh")
            self.changed()

    def open_done(self, _source, result, action, folder=False):
        try:
            Gio.AppInfo.launch_default_for_uri_finish(result)
        except GLib.Error as error:
            # Nautilus can disappear during D-Bus activation when the loaded
            # GPU driver differs from newly installed userspace libraries.
            # Retry only that failure, only for Nautilus, with software rendering.
            app = Gio.AppInfo.get_default_for_type("inode/directory", False) if folder else None
            if (app is not None and app.get_id() == "org.gnome.Nautilus.desktop"
                    and error.matches(Gio.dbus_error_quark(), Gio.DBusError.NO_REPLY)):
                print(user_error(action, error) + "; retrying Nautilus with software rendering", flush=True)
                try:
                    # A separate unit keeps the file manager outside the panel's
                    # memory/CPU limits. No shell, global environment or default
                    # application changes; normal launches still use GIO.
                    process = Gio.Subprocess.new([
                        "/usr/bin/systemd-run", "--user", "--collect", "--quiet",
                        "--expand-environment=no",
                        "--property=Type=exec", "/usr/bin/env", "GSK_RENDERER=cairo",
                        "/usr/bin/nautilus", "--new-window", self.root.as_uri(),
                    ], Gio.SubprocessFlags.NONE)
                    process.wait_check_async(None,
                        lambda source, result: self.fallback_done(source, result, action))
                    return
                except GLib.Error as fallback_error:
                    error = fallback_error
            self.report_open_error(action, error, folder)
        else:
            self.action_done(None)

    def panel_done(self, process, result):
        try:
            process.wait_check_finish(result)
        except GLib.Error as error:
            self.panel_failed(error)

    def panel_failed(self, error):
        print(user_error("Control panel launch", error), flush=True)
        self.action_done("The control panel could not start. Try again, or open " + self.client.url + " in your browser.")

    def layout(self, parent, depth, names):
        ids = ([1, 2, 3, 8, 4, 5, 6, 21, 7] if parent == 0 else
               list(range(9, 9 + RECENT_SLOTS)) if parent == 8 else [])
        children = [] if depth == 0 else [GLib.Variant("(ia{sv}av)", self.layout(i, depth - 1 if depth > 0 else -1, names)) for i in ids]
        return parent, self.properties(parent, names), children

    def setup_done(self, process, result):
        try:
            process.wait_check_finish(result)
        except GLib.Error:
            self.action_done("Could not start QNAP setup. Run the anaNAS installer to repair the setup components.")

    def fallback_done(self, process, result, action):
        try:
            process.wait_check_finish(result)
        except GLib.Error as error:
            self.report_open_error(action, error, True)
        else:
            self.action_done(None)

    def action_done(self, error):
        self.action_error = error
        if error:
            print(error, flush=True)
            self.notify_error(error)
        self.changed()

    def report_open_error(self, action, error, folder):
        # Diagnostics belong in the journal, not in desktop notifications.
        print(user_error(action, error), flush=True)
        application = "file manager" if folder else "web browser"
        if error.matches(Gio.dbus_error_quark(), Gio.DBusError.NO_REPLY):
            detail = f"The {application} stopped responding."
        elif error.matches(Gio.io_error_quark(), Gio.IOErrorEnum.PERMISSION_DENIED):
            detail = "Access to this location was denied."
        elif error.matches(Gio.io_error_quark(), Gio.IOErrorEnum.NOT_FOUND):
            detail = f"The location or {application} could not be found."
        else:
            detail = f"The {application} could not open this location."
        next_step = (f"Try again, or open {self.root} directly in your file manager."
                     if folder else f"Try again, or visit {self.client.url} in your browser.")
        self.action_done(f"{action}. {detail} {next_step}")

    def notify_error(self, message):
        if self.bus is None:
            return
        self.bus.call(
            "org.freedesktop.Notifications", "/org/freedesktop/Notifications",
            "org.freedesktop.Notifications", "Notify",
            GLib.Variant("(susssasa{sv}i)",
                         ("anaNAS", 0, "dialog-error-symbolic", "anaNAS problem",
                          str(message)[:1024], [], {}, -1)),
            None, Gio.DBusCallFlags.NONE, 5000, None, None)

    def control_worker(self):
        while not self.stop.is_set():
            paused = self.commands.get()
            error = None
            try:
                if paused == "refresh":
                    self.deliver(self.client.status(), None)
                else:
                    self.client.pause(paused)
            except (OSError, ValueError, http.client.HTTPException) as exc:
                action = ("Could not refresh anaNAS status" if paused == "refresh" else
                          "Could not change synchronization state")
                error = user_error(action, exc)
            GLib.idle_add(self.control_done, error)

    def control_done(self, error):
        self.busy = False
        self.action_done(error)
        return False

    def call(self, _conn, _sender, _path, iface, method, parameters, invocation):
        args = parameters.unpack()
        try:
            result = None
            if iface == ITEM:
                if method in ("Activate", "SecondaryActivate"):
                    self.event(6, "clicked")
            elif method == "GetLayout":
                parent, depth, names = args
                result = GLib.Variant("(u(ia{sv}av))", (self.revision, self.layout(parent, depth, names)))
            elif method == "GetGroupProperties":
                ids, names = args
                result = GLib.Variant("(a(ia{sv}))", ([(i, self.properties(i, names)) for i in ids if i in self.items() or i == 0],))
            elif method == "GetProperty":
                result = GLib.Variant("(v)", (self.properties(args[0], [args[1]])[args[1]],))
            elif method == "Event":
                self.event(args[0], args[1])
            elif method == "EventGroup":
                for item_id, event, _data, _stamp in args[0]:
                    self.event(item_id, event)
                result = GLib.Variant("(ai)", ([],))
            elif method == "AboutToShow":
                result = GLib.Variant("(b)", (False,))
            elif method == "AboutToShowGroup":
                result = GLib.Variant("(aiai)", ([], []))
            invocation.return_value(result)
        except (KeyError, ValueError, GLib.Error) as error:
            invocation.return_dbus_error("org.freedesktop.DBus.Error.InvalidArgs", str(error))

    def quit(self):
        self.stop.set()
        self.loop.quit()
        return False

    def run(self):
        GLibUnix.signal_add(GLib.PRIORITY_DEFAULT, signal.SIGTERM, self.quit)
        GLibUnix.signal_add(GLib.PRIORITY_DEFAULT, signal.SIGINT, self.quit)
        self.loop.run()
        self.stop.set()
        if self.watch_id:
            Gio.bus_unwatch_name(self.watch_id)
        Gio.bus_unown_name(self.name_id)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--control-panel", action="store_true")
    parser.add_argument("--setup", action="store_true")
    parser.add_argument("--port", type=int)
    parser.add_argument("--root", type=Path)
    parser.add_argument("--config", type=Path, default=Path(os.environ.get("XDG_CONFIG_HOME", Path.home() / ".config")) / "nas-sync/config.json")
    args = parser.parse_args()
    if args.setup:
        from ananas_setup_gui import run
        run()
        return
    if args.control_panel and args.port and args.root:
        from ananas_control_panel import run
        run(Client(args.port), args.root)
        return
    if not args.config.exists():
        if args.control_panel:
            from ananas_setup_gui import run
            run()
        else:
            Indicator(Client(args.port or 8721), args.root or Path.home() / "QNAP", configured=False).run()
        return
    with args.config.open("rb") as source:
        data = source.read(MAX_RESPONSE + 1)
    if len(data) > MAX_RESPONSE:
        raise ValueError("Config exceeds 64 KiB")
    config = json.loads(data)
    root = Path(config["local"]["root"])
    if not root.is_absolute():
        raise ValueError("Local root must be absolute")
    if args.control_panel:
        from ananas_control_panel import run
        run(Client(args.port or config["webPort"]), args.root or root)
        return
    Indicator(Client(config["webPort"]), root).run()


if __name__ == "__main__":
    main()
