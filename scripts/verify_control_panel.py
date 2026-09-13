"""Live control-panel acceptance: status, tree, submenu, reuse, pause and archive.

--before-restart records traffic for the later persistence assertion.
Default verification restores the original pause setting and renders only its
own native test window. No synchronization content is created or edited.
"""
import argparse
import hashlib
import json
from pathlib import Path
import time
import os

from nas_sync_indicator import Client
from gi.repository import Gio, GLib


def run(args):
    client = Client(8721)
    initial = client.status()
    if args.before_restart:
        args.evidence.write_text(json.dumps(initial['traffic'], indent=2) + '\n')
        return
    evidence = {'initialStatus': initial}
    started = time.monotonic()
    storage = client.storage()
    evidence['storageSeconds'] = time.monotonic() - started
    evidence['storage'] = storage
    assert storage['capacity']['available']
    assert storage['usage']['localBytes'] > 0
    assert storage['usage']['localBytes'] == storage['usage']['syncedBytes']
    assert {row['name'] for row in storage['usage']['children']} == {'SampleDocuments', '.ananas-tests'}
    if args.prior:
        prior = json.loads(args.prior.read_text())
        current = initial['traffic']
        assert prior['recordingSince'] == current['recordingSince']
        assert prior['startedAt'] != current['startedAt']
        assert current['last24Hours']['upload'] >= prior['last24Hours']['upload']
        assert current['last24Hours']['download'] >= prior['last24Hours']['download']
        evidence['trafficSurvivedRestart'] = True

    bus = Gio.bus_get_sync(Gio.BusType.SESSION, None)
    def call(dest, path, interface, method, params):
        return bus.call_sync(dest, path, interface, method, params, None, Gio.DBusCallFlags.NONE, 5000, None).unpack()
    def menu(method, params):
        return call('org.kde.StatusNotifierItem.nas_sync', '/Menu', 'com.canonical.dbusmenu', method, params)
    layout = menu('GetLayout', GLib.Variant('(iias)', (0, -1, [])))
    top = layout[1][2]
    assert [row[0] for row in top] == [1, 2, 3, 8, 4, 5, 6, 7]
    submenu = next(row for row in top if row[0] == 8)
    assert len(submenu[2]) == 12 and submenu[1]['children-display'] == 'submenu'
    evidence['topLevelRows'] = len(top)
    evidence['recentSubmenuRows'] = len(submenu[2])
    def owner():
        return call('org.freedesktop.DBus', '/org/freedesktop/DBus', 'org.freedesktop.DBus', 'GetNameOwner', GLib.Variant('(s)', ('org.ananas.ControlPanel',)))[0]
    before = owner()
    menu('Event', GLib.Variant('(isvu)', (6, 'clicked', GLib.Variant('i', 0), 0)))
    time.sleep(1)
    assert owner() == before
    evidence['sameWindowProcessOnReopen'] = True
    try:
        client.pause(not initial['paused'])
        assert client.status()['paused'] is not initial['paused']
    finally:
        client.pause(initial['paused'])
    assert client.status()['paused'] == initial['paused']
    evidence['pauseRoundTripRestored'] = True

    fixtures = {'anaNAS-live-check.bin': '1111111111111111111111111111111111111111111111111111111111111111',
                'anaNAS-folders-aaaaaaaaaaaa/inner/probe.bin': '2222222222222222222222222222222222222222222222222222222222222222'}
    for root in (Path('/home/example-user/NASdir'), Path('/mnt/nasdir')):
        for name, digest in fixtures.items():
            assert not (root / name.split('/')[0]).exists()
            assert hashlib.sha256((root / '.ananas-tests' / name).read_bytes()).hexdigest() == digest
        assert not (root / 'anaNAS-deletions-bbbbbbbbbbbb').exists()
    evidence['archiveHashesMatchBothSides'] = True

    # Render real data in an isolated verification window, not other applications.
    from ananas_control_panel import Panel, Gtk
    import cairo
    panel = Panel(client, Path('/home/example-user/NASdir'))
    panel.set_application_id('org.ananas.ControlPanel.LiveVerification')
    panel.register(None)
    panel.build()
    panel.window.show_all()
    panel.render(client.status(), None)
    panel.storage_done(storage, None)
    panel.tree.expand_row(Gtk.TreePath.new_from_string('0:0'), False)
    work, done = panel.jobs.get_nowait()
    done(work(), None)
    assert panel.tree.row_expanded(Gtk.TreePath.new_from_string('0:0'))
    evidence['firstExpansionStaysOpen'] = True
    panel.tree.emit('row-activated', Gtk.TreePath.new_from_string('0:0'), panel.tree.get_column(0))
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        while Gtk.events_pending():
            Gtk.main_iteration_do(False)
        try:
            locations = call('org.gnome.Nautilus', '/org/freedesktop/FileManager1', 'org.freedesktop.DBus.Properties', 'Get',
                             GLib.Variant('(ss)', ('org.freedesktop.FileManager1', 'OpenLocations')))[0]
            if 'file:///home/example-user/NASdir/SampleDocuments' in locations:
                break
        except GLib.Error:
            pass
        time.sleep(0.1)
    else:
        raise AssertionError('Selected SampleDocuments directory did not open')
    evidence['doubleClickOpenedSelectedDirectory'] = True
    if args.terminal:
        before_pids = {p.name for p in Path('/proc').iterdir() if p.name.isdigit()}
        panel.open_terminal('SampleDocuments')
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            while Gtk.events_pending():
                Gtk.main_iteration_do(False)
            found = None
            for process in Path('/proc').iterdir():
                if not process.name.isdigit() or process.name in before_pids:
                    continue
                try:
                    if process.stat().st_uid == os.getuid() and (process / 'comm').read_text().strip() == 'bash':
                        cwd = os.readlink(process / 'cwd')
                        if cwd == '/home/example-user/NASdir/SampleDocuments':
                            found = dict(pid=int(process.name), cwd=cwd)
                            break
                except (OSError, ProcessLookupError):
                    pass
            if found:
                evidence['terminalOpenedInSelectedDirectory'] = found
                break
            time.sleep(0.1)
        else:
            raise AssertionError('New terminal shell did not start in SampleDocuments')
    while Gtk.events_pending():
        Gtk.main_iteration_do(False)
    allocation = panel.window.get_allocation()
    surface = cairo.ImageSurface(cairo.FORMAT_ARGB32, allocation.width, allocation.height)
    panel.window.draw(cairo.Context(surface))
    surface.write_to_png(str(args.evidence.with_suffix('.png')))
    panel.show_folder_menu(Gtk.TreePath.new_from_string('0:0'))
    while Gtk.events_pending():
        Gtk.main_iteration_do(False)
    menu = panel.folder_menu.get_toplevel()
    allocation = menu.get_allocation()
    surface = cairo.ImageSurface(cairo.FORMAT_ARGB32, allocation.width, allocation.height)
    menu.draw(cairo.Context(surface))
    surface.write_to_png(str(args.evidence.with_name(args.evidence.stem + '-menu.png')))
    panel.folder_menu.popdown()
    panel.window.destroy()
    evidence['finalStatus'] = client.status()
    assert evidence['finalStatus']['syncReason'] == 'LAN sync active'
    args.evidence.write_text(json.dumps(evidence, indent=2) + '\n')
    print(json.dumps({k: v for k, v in evidence.items() if k not in ('initialStatus', 'finalStatus', 'storage')}))


if __name__ == '__main__':
    from legacy_profile import require_configured
    require_configured()
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--evidence', type=Path, required=True)
    parser.add_argument('--before-restart', action='store_true')
    parser.add_argument('--prior', type=Path)
    parser.add_argument('--terminal', action='store_true')
    run(parser.parse_args())
