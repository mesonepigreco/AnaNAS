"""Online NAS inventory tree for the native control panel."""
import math
import time
import gi
gi.require_version('Gtk', '3.0')
gi.require_version('PangoCairo', '1.0')
from gi.repository import Gtk, Gdk, GLib, GObject, Pango, PangoCairo


class SyncArrows(Gtk.CellRenderer):
    __gproperties__ = {'active': (bool, 'Active', 'Currently transferring', False, GObject.ParamFlags.READWRITE)}

    def __init__(self):
        super().__init__()
        self.active = False
        self.set_fixed_size(28, 26)

    def do_get_property(self, prop):
        return self.active

    def do_set_property(self, prop, value):
        self.active = value

    def do_render(self, cr, widget, background, cell, flags):
        if not self.active:
            return
        cr.save()
        cr.translate(cell.x + cell.width / 2, cell.y + cell.height / 2)
        cr.rotate((time.monotonic() * 2.8) % (2 * math.pi))
        cr.set_source_rgb(.12, .43, .31)
        cr.set_line_width(1.8)
        for offset in (0, math.pi):
            cr.arc(0, 0, 7, offset + .25, offset + 2.65)
            cr.stroke()
            angle = offset + 2.65
            x, y = 7 * math.cos(angle), 7 * math.sin(angle)
            tx, ty = -math.sin(angle), math.cos(angle)
            cr.move_to(x + tx * 2, y + ty * 2)
            cr.line_to(x - tx * 3 - ty * 3, y - ty * 3 + tx * 3)
            cr.line_to(x - tx * 3 + ty * 3, y - ty * 3 - tx * 3)
            cr.close_path()
            cr.fill()
        cr.restore()


class LocalProgress(Gtk.CellRenderer):
    __gproperties__ = {'percent': (int, 'Percent', 'Files synced locally', 0, 100, 0, GObject.ParamFlags.READWRITE)}

    def __init__(self):
        super().__init__()
        self.percent = 0
        self.set_fixed_size(210, 28)

    def do_get_property(self, prop):
        return self.percent

    def do_set_property(self, prop, value):
        self.percent = value

    def do_render(self, cr, widget, background, cell, flags):
        x, y, width = cell.x + 8, cell.y + (cell.height - 18) / 2, max(10, cell.width - 16)
        fraction = self.percent / 100
        # A red outline remains visible at 0%; the fill becomes green at 100%.
        red, green, blue = .82 - .66 * fraction, .22 + .40 * fraction, .24 + .10 * fraction
        cr.save()
        cr.set_source_rgb(.91, .93, .92)
        cr.rectangle(x, y, width, 18)
        cr.fill()
        cr.set_source_rgb(red, green, blue)
        cr.rectangle(x, y, max(2, width * fraction), 18)
        cr.fill()
        cr.set_line_width(1)
        cr.rectangle(x, y, width, 18)
        cr.stroke()
        layout = widget.create_pango_layout(f'{self.percent}%')
        layout.set_font_description(Pango.FontDescription('Sans Bold 9'))
        tw, th = layout.get_pixel_size()
        # White backing keeps the label readable at every red/green fill level.
        cr.set_source_rgba(1, 1, 1, .90)
        cr.rectangle(x + (width - tw) / 2 - 4, y, tw + 8, 18)
        cr.fill()
        cr.set_source_rgb(.12, .18, .15)
        cr.move_to(x + (width - tw) / 2, y + (18 - th) / 2)
        PangoCairo.show_layout(cr, layout)
        cr.restore()


def contains(directory, path):
    return bool(path) and (not directory or path == directory or path.startswith(directory + '/'))


class NASTree(Gtk.Box):
    # path, label, directory, total, local, active, special row marker
    def __init__(self, panel):
        super().__init__(orientation=Gtk.Orientation.VERTICAL, spacing=10, margin=14)
        self.panel = panel
        self.loading = set()
        self.active_path = ''
        self.priority = ''
        self.timer = 0
        self.refresh_timer = 0
        self.refresh_position = 0
        self.summary = Gtk.Label(label='Reading the NAS inventory…', xalign=0)
        self.summary.get_style_context().add_class('section')
        self.pack_start(self.summary, False, False, 0)
        note = Gtk.Label(label='Online files reported by the NAS · Progress counts files with the current version available locally.\nRight-click a folder to prioritize it after the current transfer finishes.', xalign=0)
        note.set_line_wrap(True)
        note.get_style_context().add_class('muted')
        self.pack_start(note, False, False, 0)
        self.model = Gtk.TreeStore(str, str, bool, int, int, bool, str)
        self.view = Gtk.TreeView(model=self.model)
        self.view.set_tooltip_column(0)
        self.view.set_enable_tree_lines(True)
        self.view.set_grid_lines(Gtk.TreeViewGridLines.HORIZONTAL)
        name_column = Gtk.TreeViewColumn('On the NAS')
        icon = Gtk.CellRendererPixbuf()
        name_column.pack_start(icon, False)
        name_column.set_cell_data_func(icon, self.icon_cell)
        label = Gtk.CellRendererText(ellipsize=Pango.EllipsizeMode.MIDDLE)
        name_column.pack_start(label, True)
        name_column.add_attribute(label, 'text', 1)
        name_column.set_cell_data_func(label, self.name_cell)
        arrows = SyncArrows()
        name_column.pack_start(arrows, False)
        name_column.add_attribute(arrows, 'active', 5)
        name_column.set_expand(True)
        name_column.set_resizable(True)
        name_column.set_min_width(300)
        self.view.append_column(name_column)
        bar = LocalProgress()
        progress_column = Gtk.TreeViewColumn('Synced locally')
        progress_column.pack_start(bar, True)
        progress_column.set_cell_data_func(bar, self.progress_cell)
        self.view.append_column(progress_column)
        counts = Gtk.CellRendererText(xalign=1)
        count_column = Gtk.TreeViewColumn('Local / online files')
        count_column.pack_start(counts, True)
        count_column.set_cell_data_func(counts, self.count_cell)
        self.view.append_column(count_column)
        self.view.connect('row-expanded', self.expanded)
        self.view.connect('row-activated', self.activated)
        self.view.connect('button-press-event', self.button_press)
        self.view.connect('popup-menu', self.keyboard_menu)
        scroll = Gtk.ScrolledWindow()
        scroll.set_policy(Gtk.PolicyType.AUTOMATIC, Gtk.PolicyType.AUTOMATIC)
        scroll.add(self.view)
        self.pack_start(scroll, True, True, 0)
        self.message = Gtk.Label(xalign=0)
        self.message.set_line_wrap(True)
        self.pack_start(self.message, False, False, 0)
        actions = Gtk.Box(spacing=8)
        refresh = Gtk.Button(label='Refresh NAS inventory')
        refresh.connect('clicked', lambda _: self.refresh())
        actions.pack_start(refresh, False, False, 0)
        normal = Gtk.Button(label='Clear folder priority')
        normal.connect('clicked', lambda _: self.prioritize(''))
        actions.pack_start(normal, False, False, 0)
        self.pack_start(actions, False, False, 0)
        self.root = self.model.append(None, ['', 'NAS', True, 0, 0, False, ''])
        self.model.append(self.root, ['', 'Loading…', False, 0, 0, False, 'placeholder'])
        self.connect('map', self.mapped)
        self.connect('unmap', self.unmapped)

    def icon_cell(self, _column, cell, model, node, _data=None):
        cell.set_property('icon-name', 'folder-remote-symbolic' if model[node][2] else 'text-x-generic-symbolic')
        cell.set_property('visible', not bool(model[node][6]))

    def name_cell(self, _column, cell, model, node, _data=None):
        cell.set_property('weight', Pango.Weight.BOLD if model[node][5] else Pango.Weight.NORMAL)

    def progress_cell(self, _column, cell, model, node, _data=None):
        total, local = model[node][3], model[node][4]
        cell.set_property('percent', min(100, int(100 * local / total)) if total else 100)
        cell.set_property('visible', not bool(model[node][6]))

    def count_cell(self, _column, cell, model, node, _data=None):
        cell.set_property('text', '' if model[node][6] else f'{model[node][4]:,} / {model[node][3]:,}')

    def find(self, path):
        found = []
        self.model.foreach(lambda model, _p, node, _d: found.append(node.copy()) if model[node][0] == path and not model[node][6] else None, None)
        return found[0] if found else None

    def mapped(self, *_):
        self.refresh()
        if not self.refresh_timer:
            self.refresh_timer = GLib.timeout_add_seconds(5, self.periodic_refresh)
        self.update_activity(self.active_path)

    def unmapped(self, *_):
        for name in ('timer', 'refresh_timer'):
            if getattr(self, name):
                GLib.source_remove(getattr(self, name))
                setattr(self, name, 0)

    def periodic_refresh(self):
        if not self.get_mapped():
            self.refresh_timer = 0
            return False
        self.load('')
        expanded = []
        self.model.foreach(lambda model, p, node, _d: expanded.append(model[node][0]) if model[node][0] and model[node][2] and self.view.row_expanded(p) else None, None)
        if expanded:
            self.load(expanded[self.refresh_position % len(expanded)])
            self.refresh_position += 1
        return True

    def refresh(self):
        self.load('')

    def expanded(self, _view, node, _path):
        child = self.model.iter_children(node)
        if self.model[node][2] and child and self.model[child][6] == 'placeholder':
            self.load(self.model[node][0])

    def activated(self, _view, path, _column):
        row = self.model[path]
        if row[6].startswith('more:'):
            parent = self.model.iter_parent(row.iter)
            self.load(self.model[parent][0], row[6][5:])
        elif row[2]:
            if self.view.row_expanded(path):
                self.view.collapse_row(path)
            else:
                self.view.expand_row(path, False)

    def load(self, directory, after=''):
        if directory in self.loading:
            return
        self.loading.add(directory)
        def done(value, error):
            self.loading.discard(directory)
            if error:
                self.message.set_text('NAS inventory unavailable. Showing the last received tree; refresh when connected.')
                return False
            parent = self.find(directory)
            if parent is None:
                return False
            was_expanded = self.view.row_expanded(self.model.get_path(parent))
            self.model[parent][3], self.model[parent][4] = value['files'], value['syncedFiles']
            existing = {}
            child = self.model.iter_children(parent)
            while child:
                following = self.model.iter_next(child)
                if self.model[child][6]:
                    self.model.remove(child)
                else:
                    existing[self.model[child][0]] = child.copy()
                child = following
            seen = set()
            for item in value['children']:
                p = item['path']
                seen.add(p)
                values = [p, item['name'], item['directory'], item['files'], item['syncedFiles'], contains(p, self.active_path), '']
                child = existing.get(p)
                if child is None:
                    child = self.model.append(parent, values)
                    if item['directory']:
                        self.model.append(child, ['', 'Loading…', False, 0, 0, False, 'placeholder'])
                else:
                    self.model[child] = values
            # Keep previously paged rows while the first page refreshes. A full
            # refresh of a small directory removes entries deleted on the NAS.
            if not after and not value.get('next'):
                for p, child in existing.items():
                    if p not in seen:
                        self.model.remove(child)
            if value.get('next'):
                self.model.append(parent, ['', 'Load more…', False, 0, 0, False, 'more:' + value['next']])
            if was_expanded:
                self.view.expand_row(self.model.get_path(parent), False)
            self.priority = value.get('priority', '')
            if not directory:
                partial = ' · reading inventory…' if value.get('loading') or not value['inventory']['complete'] else ''
                self.summary.set_text(f"{value['syncedFiles']:,} / {value['files']:,} NAS files synced locally{partial}")
                self.view.expand_row(self.model.get_path(self.root), False)
            problem = value.get('problem', '')
            priority = f'Priority: {self.priority} · starts after the current transfer finishes.' if self.priority else ''
            self.message.set_text(problem or priority)
            self.update_activity(value.get('activePath', ''))
            return False
        if not self.panel.submit(lambda: self.panel.client.nas_tree(directory, after), done):
            self.loading.discard(directory)

    def update_activity(self, path):
        self.active_path = path or ''
        def update(model, _p, node, _data):
            model[node][5] = not bool(model[node][6]) and contains(model[node][0], self.active_path)
        self.model.foreach(update, None)
        if self.active_path and self.get_mapped() and not self.timer:
            self.timer = GLib.timeout_add(33, self.animate)
        elif not self.active_path and self.timer:
            GLib.source_remove(self.timer)
            self.timer = 0

    def animate(self):
        if not self.get_mapped() or not self.active_path:
            self.timer = 0
            return False
        self.view.queue_draw()
        return True

    def prioritize(self, directory):
        self.message.set_text('Queuing directory priority…')
        def done(value, error):
            self.message.set_text('Could not prioritize this folder. Refresh and retry.' if error else
                                  ('Priority queued. The current transfer will finish first.' if directory else 'Folder priority cleared.'))
            if not error:
                self.priority = directory
            return False
        self.panel.submit(lambda: self.panel.client.prioritize_directory(directory), done)

    def menu(self, event=None):
        model, node = self.view.get_selection().get_selected()
        if node is None or not model[node][2] or model[node][6]:
            return False
        directory = model[node][0]
        menu = Gtk.Menu()
        item = Gtk.MenuItem(label='Prioritize syncing this directory')
        item.connect('activate', lambda _: self.prioritize(directory))
        menu.append(item)
        menu.show_all()
        self.context_menu = menu
        if event:
            menu.popup_at_pointer(event)
        else:
            menu.popup_at_widget(self.view, Gdk.Gravity.CENTER, Gdk.Gravity.CENTER, None)
        return True

    def button_press(self, _view, event):
        if event.button != 3:
            return False
        hit = self.view.get_path_at_pos(int(event.x), int(event.y))
        if hit is None:
            return False
        self.view.get_selection().select_path(hit[0])
        return self.menu(event)

    def keyboard_menu(self, _view):
        return self.menu()
