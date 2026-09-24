#!/usr/bin/python3
"""Interactive source installer. Run as your desktop user, never with sudo.

Dependencies (Debian/Ubuntu): python3-gi gir1.2-gtk-3.0 openssh-client
smbclient cifs-utils nftables policykit-1 openssl iproute2, plus Go >= 1.27.
Installs into ~/.local and launches the graphical wizard (or --cli).
"""
import argparse
import getpass
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

import ananas_setup as setup


def install_files():
    if os.getuid() == 0:
        raise setup.SetupError("Run this installer as your desktop user, not as root. It requests administrator approval only when needed.")
    for executable in ("go", "ssh", "ssh-keyscan", "ssh-keygen", "smbclient", "openssl", "ip", "nft", "mount.cifs", "pkexec", "systemctl"):
        if not shutil.which(executable):
            raise setup.SetupError(f"Missing {executable}. Install the dependencies listed in scripts/install_ananas.py, then retry.")
    repo = Path(__file__).resolve().parent.parent
    if not (repo / "go.mod").is_file():
        raise setup.SetupError("Run the installer from an anaNAS source checkout.")
    destination = setup.bundle_home()
    destination.mkdir(mode=0o700, parents=True, exist_ok=True)
    bin_dir = Path.home() / ".local/bin"
    bin_dir.mkdir(parents=True, exist_ok=True)
    environment = dict(os.environ, GOCACHE="/tmp/nas-sync-go-cache", GOMODCACHE="/tmp/nas-sync-go-mod-cache", CGO_ENABLED="0")
    with tempfile.TemporaryDirectory(prefix="ananas-build-") as temporary:
        stage = Path(temporary)
        print("Building the PC daemon and QNAP helpers…", flush=True)
        builds = [(None, "nas-sync", "nas-sync")]
        builds += [(arch, name, "ananas-helper" if name == "helper" else "ananas-helper-launch")
                   for arch in ("amd64", "arm64", "arm") for name in ("helper", "launcher")]
        for arch, name, command in builds:
            target = stage / (arch or "pc") / name
            target.parent.mkdir(parents=True, exist_ok=True)
            env = dict(environment)
            if arch:
                env.update(GOOS="linux", GOARCH=arch, GOARM="7")
            subprocess.run(["go", "build", "-trimpath", "-o", str(target), "./cmd/" + command], cwd=repo, env=env, check=True)
        # Keep a binary backup on upgrade. Replacing the file does not restart an
        # already running daemon or touch its configuration/index.
        installed = bin_dir / "nas-sync"
        if installed.exists():
            backup = installed.with_name("nas-sync.before-setup")
            if not backup.exists():
                shutil.copy2(installed, backup)
        candidate = bin_dir / "nas-sync.setup-new"
        shutil.copy2(stage / "pc/nas-sync", candidate)
        os.replace(candidate, installed)
        for arch in ("amd64", "arm64", "arm"):
            target = destination / "bin" / arch
            target.mkdir(parents=True, exist_ok=True)
            for name in ("helper", "launcher"):
                shutil.copy2(stage / arch / name, target / name)
    for name in ("ananas_setup.py", "ananas_setup_gui.py", "ananas_setup_system.py", "ananas_control_panel.py"):
        shutil.copy2(repo / "scripts" / name, bin_dir / name)
    shutil.copy2(repo / "scripts/ananas_setup_system.py", destination / "ananas_setup_system.py")
    shutil.copy2(repo / "scripts/nas_sync_indicator.py", bin_dir / "nas-sync-indicator")
    # Importable module needed by both the native panel and setup verification.
    shutil.copy2(repo / "scripts/nas_sync_indicator.py", bin_dir / "nas_sync_indicator.py")
    services = Path.home() / ".config/systemd/user"
    services.mkdir(parents=True, exist_ok=True)
    service = services / "nas-sync-indicator.service"
    if not service.exists():
        setup.private_write(service, (repo / "deploy/nas-sync-indicator.service").read_bytes(), 0o644)
    applications = Path.home() / ".local/share/applications"
    applications.mkdir(parents=True, exist_ok=True)
    # The panel item names this private directory as its icon theme path, so
    # the pineapple resolves even before the shell rescans the hicolor theme.
    for icons in (Path.home() / ".local/share/icons/hicolor/scalable/apps", Path.home() / ".local/share/anaNAS/icons"):
        icons.mkdir(parents=True, exist_ok=True)
        for icon in ("ananas.svg", "ananas-symbolic.svg"):
            shutil.copy2(repo / "internal/web/assets" / icon, icons / icon)
    main_desktop = applications / "ananas.desktop"
    if not main_desktop.exists():
        setup.private_write(main_desktop, (repo / "deploy/ananas.desktop").read_bytes(), 0o644)
    desktop = applications / "ananas-setup.desktop"
    setup.atomically(desktop, ("[Desktop Entry]\nType=Application\nName=anaNAS Setup\n"
                     "Comment=Connect your QNAP and choose folders to sync\nExec=/usr/bin/python3 " +
                     '"' + str(bin_dir / "ananas_setup_gui.py").replace('"', '\\"') + '"\nIcon=folder-remote\nCategories=Network;Utility;\n').encode())
    setup.run(["systemctl", "--user", "daemon-reload"])
    setup.run(["systemctl", "--user", "enable", "nas-sync-indicator.service"])
    # Restart so an already running panel item reloads the updated module/icons.
    setup.run(["systemctl", "--user", "restart", "nas-sync-indicator.service"])
    print("Installed. Existing sync profiles and running daemons were preserved.", flush=True)


def terminal():
    current = setup.profiles()
    for profile in current:
        print("Configured:", profile["nas"]["host"], profile["nas"]["share"], "→", profile["local"]["root"])
    print("QNAP devices:", ", ".join(device['name'] + ' (' + device['host'] + ')' for device in setup.discover()) or "none; enter the NAS IP manually")
    session = setup.Session(input("QNAP LAN IP: ").strip(), input("QNAP administrator username: ").strip(), getpass.getpass("QNAP password: "))
    try:
        fingerprint = session.host_key()
        if fingerprint:
            print("Verify this NAS host fingerprint:\n" + fingerprint)
            if input("Trust this host? [y/N] ").lower() != "y":
                return
        session.connect()
        inventory = session.inventory()
        for index, item in enumerate(inventory["shares"], 1):
            print(index, item["name"])
        if not inventory["shares"] or not inventory["accounts"]:
            raise setup.SetupError("Create an accessible share and a non-admin account in QTS, then retry.")
        share = inventory["shares"][int(input("Share number: ")) - 1]
        folder = input("New sync directory [anaNAS] (enter '-' for the entire share): ").strip() or "anaNAS"
        folder = "" if folder == "-" else folder
        for index, item in enumerate(inventory["accounts"], 1):
            print(index, item["name"])
        account = inventory["accounts"][int(input("Non-admin runtime account number: ")) - 1]
        local = input("New/empty local folder: ").strip()
        plan = setup.make_plan(session, inventory, share, folder, local, account)
        print("NAS:", plan["nasRoot"], "\nPC:", plan["localRoot"])
        if input("Install helper, save root-only SMB credential and start automatic sync? [y/N] ").lower() == "y":
            print(setup.install(session, inventory, plan, print)["message"])
    finally:
        session.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cli", action="store_true", help="use terminal prompts instead of GTK")
    parser.add_argument("--install-only", action="store_true", help="install application files without opening setup")
    args = parser.parse_args()
    install_files()
    if args.cli:
        terminal()
    elif not args.install_only:
        from ananas_setup_gui import run
        run()


if __name__ == "__main__":
    try:
        main()
    except (setup.SetupError, ValueError, IndexError, subprocess.SubprocessError) as error:
        print(str(error) if isinstance(error, setup.SetupError) else "Installation did not complete. Check dependencies and your selections; existing sync profiles were preserved.")
        raise SystemExit(1)
