#!/usr/bin/python3
"""LAN-only setup backend. No GTK imports; also used by the terminal installer.

Passwords never enter argv, logs, profiles or NAS configuration. An owned 0700
temporary directory holds the SSH askpass response until authentication finishes.
Only the explicitly approved SMB credential is retained, by the root installer.
"""
import concurrent.futures
import configparser
import hashlib
import http.client
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import shutil
import socket
import ssl
import subprocess
import tempfile
import time
import xml.etree.ElementTree as ET
from pathlib import PurePosixPath


class SetupError(Exception):
    """An actionable message safe to display (never include command input)."""


def command_error(program, diagnostic, code):
    """Classify known diagnostics, never display untrusted/secret command output."""
    diagnostic = diagnostic.lower()
    if 'host key verification failed' in diagnostic or 'remote host identification has changed' in diagnostic:
        return "The NAS identity could not be verified. Check its host-key fingerprint before reconnecting."
    if 'connection refused' in diagnostic:
        return "The NAS refused the connection. Enable SSH in QTS (Control Panel → Network & File Services → Telnet / SSH), then try Connect again."
    if 'permission denied' in diagnostic or 'authentication failed' in diagnostic or 'logon_failure' in diagnostic:
        return "The QNAP did not accept this login. Check your username and password, and make sure this account is allowed to use SSH."
    if 'timed out' in diagnostic or 'no route to host' in diagnostic or 'network is unreachable' in diagnostic:
        return "The QNAP is not reachable. Check that it is awake and connected to the same LAN, then try again."
    if program == 'ssh-keyscan':
        return "Could not contact SSH on this QNAP. Check its IP address and enable SSH in QTS, then retry."
    if program == 'ssh':
        return "The QNAP connection ended before this step completed. Reconnect to the NAS and try again."
    if program == 'pkexec':
        return "PC administrator approval was cancelled or system setup failed. No existing sync instance was replaced."
    return f"{program} could not complete this setup step. Check its installation and the NAS connection, then retry."


def config_home():
    return Path(os.environ.get("XDG_CONFIG_HOME", Path.home() / ".config")) / "nas-sync"


def bundle_home():
    return Path.home() / ".local/share/ananas/setup"


def run(argv, *, data=None, timeout=30, env=None):
    try:
        result = subprocess.run(argv, input=data, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, timeout=timeout, env=env)
    except subprocess.TimeoutExpired:
        raise SetupError("The operation timed out. Check the LAN connection and try again.") from None
    except FileNotFoundError:
        raise SetupError(f"Required program is missing: {Path(argv[0]).name}. Run the installer again.") from None
    if result.returncode:
        raise SetupError(command_error(Path(argv[0]).name, result.stderr[:65536].decode(errors='replace'), result.returncode))
    if len(result.stdout) > 16 * 1024 * 1024:
        raise SetupError("The device returned an unexpectedly large response.")
    return result.stdout


def private_write(path, data, mode=0o600):
    path = Path(path)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, mode)
    with os.fdopen(fd, "wb") as output:
        output.write(data)
        output.flush()
        os.fsync(output.fileno())


def atomically(path, data):
    path = Path(path)
    temporary = path.with_name(path.name + "." + secrets.token_hex(8))
    private_write(temporary, data)
    os.replace(temporary, path)


def identifier(value):
    if not isinstance(value, str) or not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}", value):
        raise SetupError("Use 1–64 letters, numbers, dots, underscores or hyphens for the folder name.")
    return value


def lan(host):
    try:
        address = ipaddress.IPv4Address(host)
    except ipaddress.AddressValueError:
        raise SetupError("Enter the QNAP's local IPv4 address, for example 10.23.42.30.") from None
    if not address.is_private or address.is_loopback or address.is_link_local or address.is_unspecified:
        raise SetupError("Setup requires a private, directly connected LAN address.")
    routes = json.loads(run(["ip", "-j", "-4", "route", "get", str(address)]))
    if len(routes) != 1 or routes[0].get("gateway") or routes[0].get("via"):
        raise SetupError("The QNAP must be on this computer's direct LAN, not through a router or VPN.")
    route = routes[0]
    device, source = route.get("dev", ""), route.get("prefsrc", "")
    interface = Path("/sys/class/net") / device
    if not (interface / "device").exists() or "/virtual/" in str(interface.resolve()):
        raise SetupError("Select a NAS reachable through a physical Ethernet or Wi-Fi interface.")
    addresses = json.loads(run(["ip", "-j", "-4", "address", "show", "dev", device]))
    if len(addresses) != 1 or "UP" not in addresses[0].get("flags", []):
        raise SetupError("The LAN interface is not connected.")
    for item in addresses[0].get("addr_info", []):
        if item.get("local") == source:
            prefix = ipaddress.ip_network(f"{source}/{item['prefixlen']}", strict=False)
            if address in prefix and str(address) != source:
                return {"host": str(address), "source": source, "interface": device, "prefix": str(prefix)}
    raise SetupError("The NAS and computer do not share a directly connected IPv4 subnet.")


def qnap_identity(raw):
    if len(raw) > 65536 or b'<!DOCTYPE' in raw.upper() or b'<!ENTITY' in raw.upper():
        return None
    try:
        root = ET.fromstring(raw)
    except ET.ParseError:
        return None
    # QNAP's QTS authentication endpoint exposes these device fields without
    # credentials. An open TCP port or an HTML page is not a QNAP identity.
    if root.tag != 'QDocRoot' or root.find('hostname') is None or root.find('webAccessPort') is None or root.find('doQuick') is None:
        return None
    name = (root.findtext('hostname') or 'QNAP').strip()
    return ''.join(c for c in name if c.isprintable())[:100] or 'QNAP'


def identify_qnap(host, route=None):
    route = route or lan(host)
    for port, secure in ((8080, False), (443, True), (80, False)):
        connection = None
        try:
            options = dict(timeout=0.7, source_address=(route['source'], 0))
            if secure:
                # This is only an unauthenticated product-discovery hint. No
                # username, password, cookies or tokens are ever sent here.
                connection = http.client.HTTPSConnection(host, port, context=ssl._create_unverified_context(), **options)
            else:
                connection = http.client.HTTPConnection(host, port, **options)
            connection.request('GET', '/cgi-bin/authLogin.cgi', headers={'Connection': 'close'})
            response = connection.getresponse()
            if response.status != 200:
                continue  # Never follow redirects off the probed endpoint.
            name = qnap_identity(response.read(65537))
            if name:
                return {'host': host, 'name': name, 'port': port}
        except (OSError, http.client.HTTPException, ValueError):
            pass
        finally:
            if connection:
                connection.close()
    return None


def discover():
    """One bounded sweep, returning QNAP product responses only (no login)."""
    candidates = set()
    for profile in profiles():
        if profile.get("nas", {}).get("host"):
            candidates.add(profile["nas"]["host"])
    addresses = json.loads(run(["ip", "-j", "-4", "address", "show", "up"]))
    for device in addresses:
        if not (Path("/sys/class/net") / device["ifname"] / "device").exists():
            continue
        for address in device.get("addr_info", []):
            if address.get("family") != "inet":
                continue
            net = ipaddress.ip_network(f"{address['local']}/{address['prefixlen']}", strict=False)
            if net.is_private and net.num_addresses <= 1024:
                candidates.update(str(ip) for ip in net.hosts() if str(ip) != address["local"])
    # Large subnets: probe known neighbours instead of sweeping thousands of hosts.
    for item in json.loads(run(["ip", "-j", "-4", "neigh", "show"])):
        if item.get("dst"):
            candidates.add(item["dst"])
    def probe(host):
        try:
            route = lan(host)
            for port in (8080, 443, 80):
                try:
                    with socket.create_connection((host, port), timeout=0.15, source_address=(route['source'], 0)):
                        return identify_qnap(host, route)
                except OSError:
                    pass
        except (SetupError, ValueError):
            pass
    with concurrent.futures.ThreadPoolExecutor(max_workers=16) as workers:
        found = workers.map(probe, sorted(candidates)[:1024])
        return sorted((device for device in found if device), key=lambda device: ipaddress.IPv4Address(device['host']))


def profiles():
    base = config_home()
    paths = [base / "config.json", *sorted((base / "profiles").glob("*/config.json"))]
    result = []
    for path in paths:
        if not path.is_file():
            continue
        try:
            if path.stat().st_size > 65536:
                raise ValueError()
            profile = json.loads(path.read_bytes())
            profile["_config"] = str(path)
            profile["_service"] = "nas-sync.service" if path.parent == base else f"ananas-sync-{path.parent.name}.service"
            result.append(profile)
        except (ValueError, OSError):
            raise SetupError(f"Cannot read the existing configuration at {path}. It has not been changed.") from None
    if result and result[0]["_config"] == str(base / "config.json"):
        duplicate = next((p for p in result[1:] if p.get("stateDir") and p.get("stateDir") == result[0].get("stateDir")), None)
        if duplicate:
            result.pop(0)
    return result


def validate_local(path, existing=None):
    root = Path(path).expanduser()
    if not root.is_absolute() or root.resolve() != root or root == Path.home() or root == Path("/"):
        raise SetupError("Choose an absolute, non-symlink folder, not your entire home or filesystem.")
    # An attempt that never finished synchronizes nothing, so it must not reserve a
    # folder name; the setup that replaces it discards its record first.
    for profile in [item for item in profiles() if not incomplete_attempt(item)] if existing is None else existing:
        other = Path(profile["local"]["root"])
        if root == other or root in other.parents or other in root.parents:
            raise SetupError(f"This overlaps an existing sync folder: {other}. Choose a separate folder.")
    for other in (config_home(), Path.home() / ".local/state/nas-sync", bundle_home()):
        if root == other or root in other.parents or other in root.parents:
            raise SetupError("The sync folder must be separate from anaNAS settings and private state.")
    if root.exists() and (not root.is_dir() or any(root.iterdir())):
        raise SetupError("Choose a new or empty local folder. Existing non-empty folders are not merged during setup.")
    return root


def relative_directory(value):
    if not isinstance(value, str) or any(ord(c) < 32 for c in value) or '\\' in value:
        raise SetupError('This directory name is not supported for synchronization.')
    path = PurePosixPath(value)
    if path.is_absolute() or '..' in path.parts or (value and str(path) != value):
        raise SetupError('Select a directory inside the shared folder.')
    return value


def sync_tag(path, managed):
    exact = next((item for item in managed if item['root'] == path), None)
    if exact:
        return f"Configured with anaNAS · {len(exact.get('peers', []))} device(s)"
    if any(item['root'].startswith(path.rstrip('/') + '/') for item in managed):
        return 'Contains anaNAS sync folders'
    if any(path.startswith(item['root'].rstrip('/') + '/') for item in managed):
        return 'Inside an anaNAS sync folder'
    return 'Not configured with anaNAS'


def folder_order(item, managed):
    path = item['path']
    configured = any(entry['root'] == path or entry['root'].startswith(path.rstrip('/') + '/') for entry in managed)
    return (not configured, not item.get('directory', True), item['name'].casefold())


def browser_roots(inventory):
    """Promote nested configured roots without hiding their ordinary share."""
    rows = []
    for managed in inventory['managed']:
        for index, share in enumerate(inventory['shares']):
            prefix = share['path'].rstrip('/') + '/'
            if managed['root'].startswith(prefix):
                relative = managed['root'][len(prefix):]
                rows.append(dict(name=share['name'] + '/' + relative, path=managed['root'],
                                 relative=relative, share_index=index, kind='Sync folder'))
                break
    rows += [dict(name=share['name'], path=share['path'], relative='', share_index=index, kind='Shared folder')
             for index, share in enumerate(inventory['shares'])]
    return sorted(rows, key=lambda item: (not any(m['root'] == item['path'] for m in inventory['managed']),
                                          *folder_order(item, inventory['managed'])))


def local_profile(host, root, shares):
    for profile in profiles():
        if profile.get('nas', {}).get('host') != host:
            continue
        record = Path(profile['_config']).with_name('setup.json')
        if record.exists():
            target = json.loads(record.read_bytes()).get('plan', {}).get('nasRoot')
        else:
            target = next((share['path'] for share in shares if share['name'] == profile['nas']['share']), None)
        if target == root:
            return profile
    return None


class Session:
    def __init__(self, host, username, password):
        self.route = lan(host)
        self.host = self.route["host"]
        if not username:
            raise SetupError('Enter your QNAP username before connecting.')
        self.username = identifier(username)
        if not password:
            raise SetupError('Enter your QNAP password before connecting.')
        if any(char in password for char in ("\n", "\r", "\0")):
            raise SetupError("Enter a password without line breaks.")
        self.password = password
        self.temporary = tempfile.TemporaryDirectory(prefix="ananas-login-")
        self.directory = Path(self.temporary.name)
        self.connected = False
        self.root = False

    def host_key(self):
        lan(self.host)
        key = run(["ssh-keyscan", "-4", "-T", "4", "-t", "ed25519,rsa", self.host], timeout=12)
        if not key.strip():
            raise SetupError("SSH is unavailable. In QTS, enable Control Panel → Network & File Services → Telnet / SSH, then retry.")
        private_write(self.directory / "hostkey", key)
        fingerprint = run(["ssh-keygen", "-lf", str(self.directory / "hostkey"), "-E", "sha256"]).decode().strip()
        known = config_home() / "known_hosts"
        if known.exists():
            old = run(["ssh-keygen", "-F", self.host, "-f", str(known)]) if self.host in known.read_text() else b""
            old_keys = {tuple(line.split()[1:3]) for line in old.decode().splitlines() if not line.startswith("#")}
            new_keys = {tuple(line.split()[1:3]) for line in key.decode().splitlines()}
            if old_keys and not old_keys.intersection(new_keys):
                raise SetupError("The NAS host key has changed. Verify its identity before removing the old key from anaNAS known_hosts.")
            if old_keys:
                # Trust only previously approved keys, not additional keys from
                # this unauthenticated scan alongside a copied public key.
                atomically(self.directory / "hostkey", old)
                return None
        return fingerprint

    def connect(self):
        lan(self.host)
        # Caller has explicitly approved an unknown key before entering here.
        secret = self.directory / "password"
        private_write(secret, self.password.encode())
        askpass = self.directory / "askpass"
        private_write(askpass, ("#!/usr/bin/python3\nfrom pathlib import Path\n"
                               f"print(Path({str(secret)!r}).read_text(), end='')\n").encode(), 0o700)
        env = dict(os.environ, SSH_ASKPASS=str(askpass), SSH_ASKPASS_REQUIRE="force", DISPLAY=os.environ.get("DISPLAY", ":0"))
        try:
            run(["ssh", "-F", "/dev/null", "-M", "-S", str(self.directory / "control"),
                 "-o", "ControlPersist=600", "-o", "StrictHostKeyChecking=yes",
                 "-o", f"UserKnownHostsFile={self.directory / 'hostkey'}",
                 "-o", "NumberOfPasswordPrompts=1", "-o", "ConnectTimeout=8",
                 "-o", "PreferredAuthentications=password,keyboard-interactive",
                 "-o", "PubkeyAuthentication=no", "-o", "ClearAllForwardings=yes",
                 "-o", "ForwardAgent=no", "-o", f"BindInterface={self.route['interface']}",
                 "-b", self.route["source"], "-fN", f"{self.username}@{self.host}"], env=env)
        except SetupError:
            raise
        finally:
            secret.unlink(missing_ok=True)
            askpass.unlink(missing_ok=True)
        self.connected = True
        self.root = self.command("id -u").strip() == "0"
        if not self.root:
            try:
                if self.command("id -u", admin=True).strip() != "0":
                    raise SetupError("not administrator")
            except SetupError:
                raise SetupError("This account cannot install the helper. Sign in with a QNAP administrator account with SSH/sudo access.") from None
        self.command("test -x /sbin/getcfg; test -f /etc/config/uLinux.conf", admin=True)
        known = config_home() / "known_hosts"
        known.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        old = known.read_bytes() if known.exists() else b""
        atomically(known, old + (self.directory / "hostkey").read_bytes())

    def command(self, command, *, admin=False, data=b"", timeout=40):
        if lan(self.host) != self.route:
            raise SetupError("The LAN configuration changed. Reconnect before continuing setup.")
        if admin and not self.root:
            command = "sudo -S -p '' /bin/sh -c " + shlex.quote(command)
            data = self.password.encode() + b"\n" + data
        argv = ["ssh", "-F", "/dev/null", "-S", str(self.directory / "control"),
                "-o", "BatchMode=yes", "-o", "ProxyCommand=false", "-o", "ClearAllForwardings=yes",
                f"{self.username}@{self.host}", command]
        return run(argv, data=data, timeout=timeout).decode()

    def inventory(self):
        raw = self.command("head -c 262144 /etc/config/smb.conf", admin=True)
        parser = configparser.ConfigParser(interpolation=None, strict=False)
        parser.read_string(raw)
        shares = []
        for name in parser.sections():
            path = parser.get(name, "path", fallback="")
            if name.lower() == "global" or not path.startswith("/share/"):
                continue
            if any(c in name for c in "/\\\n\r\0"):
                continue
            # Administrative SMB access is checked below, rather than inferring
            # permissions from QTS ACL display strings.
            shares.append({"name": name, "path": path})
        if len(shares) > 256:
            raise SetupError("The NAS has more than 256 shares. This setup version cannot enumerate them safely.")
        credential = self.directory / "smb-auth"
        private_write(credential, f"username={self.username}\npassword={self.password}\n".encode())
        try:
            accessible = []
            deadline = time.monotonic() + 45
            for share in shares:
                if time.monotonic() > deadline:
                    raise SetupError("Share discovery took too long. Check the NAS connection and retry; no partial folder list has been accepted.")
                try:
                    run(["smbclient", f"//{self.host}/{share['name']}", "-I", self.host,
                         "-A", str(credential), "--client-protection=encrypt", "-c", "pwd"], timeout=8)
                    canonical = self.command("cd " + shlex.quote(share["path"]) + " && pwd -P", admin=True).strip()
                    if not re.fullmatch(r"/share/[A-Za-z0-9_.-]+/[^\n\r]+", canonical):
                        continue
                    share["path"] = canonical
                    accessible.append(share)
                except SetupError:
                    continue
        finally:
            credential.unlink(missing_ok=True)
        accounts = []
        for row in self.command("head -c 65536 /etc/passwd", admin=True).splitlines():
            fields = row.split(":")
            if len(fields) == 7 and fields[2].isdigit() and fields[3].isdigit() and 500 <= int(fields[2]) < 65534 and int(fields[3]) > 0:
                accounts.append({"name": fields[0], "uid": int(fields[2]), "gid": int(fields[3])})
        managed = []
        # Read only anaNAS QPKG entries, including the historical deployment.
        packages = configparser.ConfigParser(interpolation=None, strict=False)
        packages.read_string(self.command("head -c 262144 /etc/config/qpkg.conf", admin=True))
        for name in packages.sections():
            if not name.lower().startswith("ananas"):
                continue
            base = packages.get(name, "Install_Path", fallback="")
            if not base.startswith("/share/") or "\n" in base:
                continue
            try:
                profile = json.loads(self.command("head -c 65536 " + shlex.quote(base + "/helper.json"), admin=True))
                managed.append({"name": name, "base": base, "root": profile["root"],
                                "port": int(profile["listen"].rsplit(":", 1)[1]),
                                "peers": profile["peers"]})
            except (SetupError, ValueError, KeyError):
                raise SetupError("An existing anaNAS helper could not be inspected. It has not been changed.") from None
        accessible.sort(key=lambda item: folder_order(item, managed))
        self.shares = accessible
        return {"shares": accessible, "accounts": accounts, "managed": managed,
                "architecture": self.command("uname -m").strip(),
                "hostname": self.command("hostname").strip(),
                "model": self.command("/sbin/getcfg System Model -f /etc/config/uLinux.conf").strip()}

    def browse(self, share, relative=''):
        """Immediate names only, on demand, within a share authorized by inventory.

        NUL records preserve spaces/newlines. Symlinks are displayed but cannot
        be expanded or selected; no recursive traversal or content reads occur.
        """
        if share not in getattr(self, 'shares', []):
            raise SetupError('Reconnect to refresh access to this shared folder.')
        relative = relative_directory(relative)
        path = share['path'] + ('/' + relative if relative else '')
        q = shlex.quote
        script = 'set -eu\ncd ' + q(path) + '\n[ "$(pwd -P)" = ' + q(path) + ' ] || exit 41\n' + '''count=0
for child in ./* ./.[!.]* ./..?*; do
 [ -e "$child" ] || [ -L "$child" ] || continue
 count=$((count + 1))
 if [ "$count" -gt 1000 ]; then printf 'more\\0\\0\\0'; break; fi
 name=${child#./}
 if [ -L "$child" ]; then kind=link; size=0
 elif [ -d "$child" ]; then kind=directory; size=0
 elif [ -f "$child" ]; then kind=file; size=$(/bin/busybox stat -c %s "$child")
 else kind=special; size=0; fi
 printf '%s\\0%s\\0%s\\0' "$kind" "$name" "$size"
done
'''
        try:
            raw = self.command(script, admin=True, timeout=15)
        except SetupError:
            raise SetupError('Could not read this folder. Check its permissions and the NAS connection; you can retry by expanding it again.') from None
        fields = raw.split('\0')
        if fields[-1] != '' or (len(fields) - 1) % 3:
            raise SetupError('The NAS returned an incomplete folder listing. Please retry.')
        entries, more = [], False
        for offset in range(0, len(fields) - 1, 3):
            kind, name, size = fields[offset:offset + 3]
            if kind == 'more':
                more = True
                continue
            if kind not in ('directory', 'file', 'link', 'special') or not name or '/' in name or name in ('.', '..') or not size.isdigit():
                raise SetupError('The NAS returned an invalid folder entry. Please retry.')
            child = relative + '/' + name if relative else name
            entries.append({'name': name, 'relative': child, 'path': path + '/' + name,
                            'directory': kind == 'directory', 'kind': kind, 'bytes': int(size)})
        return {'entries': entries, 'truncated': more}

    def close(self):
        if self.connected:
            try:
                run(["ssh", "-F", "/dev/null", "-S", str(self.directory / "control"), "-O", "exit",
                     f"{self.username}@{self.host}"], timeout=5)
            except SetupError:
                pass
        self.password = ""
        self.temporary.cleanup()


def service_text(binary, config):
    def quote(value):
        return '"' + str(value).replace('\\', '\\\\').replace('"', '\\"').replace('%', '%%').replace('$', '$$') + '"'
    return ("[Unit]\nDescription=anaNAS folder synchronization\n[Service]\nExecStart=" +
            quote(binary) + " -config " + quote(config) + "\nRestart=on-failure\nRestartSec=30\n"
            "Environment=ANANAS_CPU_GOVERNOR=1\nCPUQuota=100%\nMemoryHigh=128M\nMemoryMax=256M\n"
            "UMask=0077\nNoNewPrivileges=true\n[Install]\nWantedBy=default.target\n")


def certificates(directory, host, source):
    def openssl(*args):
        return run(["openssl", *args], timeout=20)
    ca = str(directory / "ca")
    openssl("req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256", "-nodes",
            "-keyout", ca + ".key", "-out", ca + ".pem", "-subj", "/CN=anaNAS setup CA", "-days", "3650")
    pins = {}
    for name, usage, ip in (("server", "serverAuth", host), ("client", "clientAuth", source)):
        stem = str(directory / name)
        openssl("req", "-new", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256", "-nodes",
                "-keyout", stem + ".key", "-out", stem + ".csr", "-subj", "/CN=anaNAS " + name)
        private_write(stem + ".ext", f"subjectAltName=IP:{ip}\nextendedKeyUsage={usage}\nkeyUsage=digitalSignature\n".encode())
        openssl("x509", "-req", "-in", stem + ".csr", "-CA", ca + ".pem", "-CAkey", ca + ".key",
                "-CAcreateserial", "-out", stem + ".pem", "-days", "365", "-extfile", stem + ".ext")
        pins[name] = hashlib.sha256(openssl("x509", "-in", stem + ".pem", "-outform", "DER")).hexdigest()
    for path in directory.iterdir():
        path.chmod(0o600)
    return pins


def make_plan(session, inventory, share, folder, local, account, *, relative=''):
    """Pure validation before any installation, share or service modification."""
    if share not in inventory["shares"] or account not in inventory["accounts"]:
        raise SetupError("Select an accessible share and a non-administrator runtime account.")
    relative = relative_directory(relative)
    root = share["path"] + ('/' + relative if relative else '') + ("/" + identifier(folder) if folder else "")
    for managed in inventory["managed"]:
        other = managed["root"].rstrip("/")
        if root == other or root.startswith(other + "/") or other.startswith(root + "/"):
            raise SetupError("This directory is already synchronized by an anaNAS helper. To synchronize it on this PC too, add this PC to that helper instead of installing a second one.")
    local = validate_local(local)
    key = hashlib.sha256((session.host + "/" + root).encode()).hexdigest()[:16]
    base = str(Path(share["path"]).parent / (".ananas-" + key))
    used = {item["port"] for item in inventory["managed"]}
    used.update(p.get("sync", {}).get("port") for p in profiles())
    port = next((value for value in range(18742, 18842) if value not in used), None)
    if port is None:
        raise SetupError("No free anaNAS helper port is available.")
    web_used = {p.get("webPort") for p in profiles()}
    web = next(value for value in range(8721, 8821) if value not in web_used)
    profile = config_home() / "profiles" / key
    if profile.exists():
        raise SetupError("A previous setup attempt exists for this folder. Its saved recovery record must be inspected before retrying.")
    return {"id": key, "host": session.host, "route": session.route, "share": share["name"],
            "nasRoot": root, "createFolder": bool(folder), "localRoot": str(local),
            "base": base, "account": account, "port": port, "webPort": web,
            "profile": str(profile), "mountPoint": "/mnt/ananas-" + key,
            "uid": os.getuid(), "gid": os.getgid(), "username": __import__('pwd').getpwuid(os.getuid()).pw_name}


def remote(session, description, command, **arguments):
    """One NAS command, reported with the step that failed rather than as a bare
    connection error. Setup messages stay free of command output."""
    try:
        return session.command(command, **arguments)
    except SetupError as error:
        raise SetupError(f"The NAS step '{description}' did not complete. {error}") from None


def managed_helper(inventory, path):
    """The installed helper whose synchronized root is exactly this directory."""
    path = path.rstrip("/")
    return next((item for item in inventory["managed"] if item["root"].rstrip("/") == path), None)


def read_helper(session, managed):
    """Read an installed helper's configuration and trust material, changing nothing."""
    base = managed["base"]
    if not base.startswith("/share/") or "\n" in base or base != str(PurePosixPath(base)):
        raise SetupError("This helper's installation path is not supported for adding a PC.")
    q = shlex.quote
    try:
        config = json.loads(session.command("head -c 65536 " + q(base + "/helper.json"), admin=True))
        certificate = config["certificate"]
        client_ca = config["clientCA"]
        if not isinstance(certificate, str) or not isinstance(client_ca, str):
            raise ValueError()
    except (ValueError, KeyError, TypeError):
        raise SetupError("The installed anaNAS helper configuration could not be read. It has not been changed.") from None
    service = session.command("head -c 65536 " + q(base + "/service.sh"), admin=True)
    rules = firewall_rules(service, config.get("peers", []))
    if not rules:
        raise SetupError("The installed helper's startup script does not grant its PC a recognizable firewall rule, "
                         "so this PC cannot be added automatically. It has not been changed.")
    saved = {}
    for name, path in (("config", base + "/helper.json"), ("ca", client_ca), ("service", base + "/service.sh")):
        # Present only when an earlier attempt from some PC saved the originals.
        if session.command("test -e " + q(path + ".before-enroll") + " && echo yes || true", admin=True).strip() == "yes":
            saved[name] = session.command("head -c 65536 " + q(path + ".before-enroll"), admin=True)
    try:
        original = json.loads(saved["config"]) if "config" in saved else None
    except ValueError:
        original = None
    return {"config": config, "service": service, "rules": rules, "original": original,
            "originalCA": saved["ca"].encode() if "ca" in saved else None,
            "originalService": saved.get("service"),
            "ca": session.command("head -c 65536 " + q(client_ca), admin=True).encode(),
            "server": session.command("head -c 65536 " + q(certificate), admin=True).encode()}


def firewall_rules(service, peers):
    """The startup script's per-PC accept rules, whichever chains and directions it
    uses. A new PC is granted exactly what an enrolled PC already has, no more."""
    rules = []
    for match in re.finditer(r"^([ \t]*)(/sbin/iptables -A ([A-Za-z0-9_-]{1,28}) [^\n]*-j ACCEPT)[ \t]*$", service, re.M):
        peer = next((item for item in peers if re.search(r"(?<![0-9.])" + re.escape(item) + r"(?![0-9.])", match.group(2))), None)
        if peer:
            rules.append({"indent": match.group(1), "text": match.group(2), "chain": match.group(3),
                          "peer": peer, "end": match.end()})
    return rules


def join_certificates(directory, source):
    """A PC-local issuer and client leaf. The NAS additionally trusts this issuer,
    so no existing private key ever leaves the PC that first installed the helper."""
    def openssl(*args):
        return run(["openssl", *args], timeout=20)
    ca = str(directory / "client-ca")
    openssl("req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256", "-nodes",
            "-keyout", ca + ".key", "-out", ca + ".pem", "-days", "3650",
            "-subj", "/CN=anaNAS client CA " + secrets.token_hex(8))
    stem = str(directory / "client")
    openssl("req", "-new", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256", "-nodes",
            "-keyout", stem + ".key", "-out", stem + ".csr", "-subj", "/CN=anaNAS client")
    private_write(stem + ".ext", f"subjectAltName=IP:{source}\nextendedKeyUsage=clientAuth\nkeyUsage=digitalSignature\n".encode())
    openssl("x509", "-req", "-in", stem + ".csr", "-CA", ca + ".pem", "-CAkey", ca + ".key",
            "-CAcreateserial", "-out", stem + ".pem", "-days", "365", "-extfile", stem + ".ext")
    pin = hashlib.sha256(openssl("x509", "-in", stem + ".pem", "-outform", "DER")).hexdigest()
    for path in directory.iterdir():
        path.chmod(0o600)
    return pin


def state_home(identifier):
    return Path.home() / ".local/state/nas-sync" / identifier


def release_state(identifier):
    """Remove synchronization state that no configured profile claims. The index is
    bound to the local folder that created it, so state left by an abandoned attempt
    makes the next one fail with a mismatched root."""
    state = state_home(identifier)
    if state.exists() and not any(Path(profile.get("stateDir", "")) == state for profile in profiles()):
        shutil.rmtree(state, ignore_errors=True)


def incomplete_attempt(profile):
    """True when a profile's saved record shows setup never finished."""
    record = Path(profile["_config"]).with_name("setup.json") if isinstance(profile, dict) else Path(profile) / "setup.json"
    try:
        if record.stat().st_size > 65536:
            return True
        return json.loads(record.read_bytes()).get("phase") != "started"
    except FileNotFoundError:
        return False
    except (OSError, ValueError):
        return True


def discard_attempt(profile):
    """Remove an unfinished local setup so it can be started again. The NAS, its
    helper and the local folder's contents are not touched."""
    directory = Path(profile["_config"]).parent if isinstance(profile, dict) else Path(profile)
    if directory.parent != config_home() / "profiles" or not incomplete_attempt(directory):
        raise SetupError("Only an unfinished setup for an additional folder can be discarded here.")
    identifier = directory.name
    service = "ananas-sync-" + identifier + ".service"
    unit = Path.home() / ".config/systemd/user" / service
    if unit.exists():
        for arguments in (["disable", "--now", service], ["daemon-reload"]):
            try:
                run(["systemctl", "--user", *arguments])
            except SetupError:
                pass
        unit.unlink(missing_ok=True)
    shutil.rmtree(directory)
    release_state(identifier)


def staged_attempt(profile):
    """True when a saved attempt stopped before the NAS was changed."""
    try:
        record = Path(profile) / "setup.json"
        if record.stat().st_size > 65536:
            return False
        return json.loads(record.read_bytes()).get("phase") == "staging"
    except (OSError, ValueError):
        return False


def make_join_plan(session, inventory, share, local, managed, helper):
    """Pure validation before this PC is added to an installed helper."""
    if share not in inventory["shares"]:
        raise SetupError("Select the accessible share that contains this synchronized folder.")
    config, root = helper["config"], managed["root"]
    if not root.startswith(share["path"].rstrip("/") + "/") and root != share["path"].rstrip("/"):
        raise SetupError("This synchronized folder is not inside the selected share. Choose the share that contains it.")
    source = session.route["source"]
    original = helper.get("original")
    # An unfinished attempt from this PC left its address behind. The saved originals
    # let that attempt be undone and redone cleanly instead of refusing forever.
    repair = bool(source in config.get("peers", []) and original and source not in original.get("peers", []))
    if source in config.get("peers", []) and not repair:
        raise SetupError("This PC's address is already allowed by that helper. If sync is not configured here, remove the stale entry on the NAS before retrying.")
    if len(config.get("peers", [])) >= 64 or len(config.get("clients", {})) >= 64:
        raise SetupError("That helper already has the maximum number of enrolled PCs.")
    try:
        prefix = ipaddress.IPv4Network(config["prefix"])
        if ipaddress.IPv4Address(source) not in prefix:
            raise SetupError("This PC is not on the subnet that helper serves. Connect both machines to the same directly connected LAN.")
        port = int(config["listen"].rsplit(":", 1)[1])
        if not 1024 <= port < 65536:
            raise SetupError("That helper listens on a port this PC cannot be configured to reach.")
        namespace = config["namespace"]
        account = {"uid": int(config["uid"]), "gid": int(config["gid"])}
    except (KeyError, ValueError, ipaddress.AddressValueError):
        raise SetupError("The installed helper configuration is incomplete. It has not been changed.") from None
    if not re.fullmatch(r"[0-9a-f]{64}", str(namespace)):
        raise SetupError("The installed helper has no usable synchronization namespace. It has not been changed.")
    local = validate_local(local)
    key = hashlib.sha256((session.host + "/" + root).encode()).hexdigest()[:16]
    profile = config_home() / "profiles" / key
    # The helper's own peer list is checked above, so an attempt recorded as staged
    # changed nothing on the NAS and can simply be replaced. A later phase means the
    # NAS was touched, and that record is for a person to read before retrying.
    if profile.exists() and staged_attempt(profile):
        shutil.rmtree(profile)
    if profile.exists():
        raise SetupError("A previous setup attempt for this folder reached the NAS. Inspect its saved recovery record at "
                         + str(profile / "setup.json") + " before retrying.")
    release_state(key)
    if any(p.get("sync", {}).get("port") == port and p.get("nas", {}).get("host") == session.host for p in profiles()):
        raise SetupError("This PC already has a profile for that helper. Open its control panel instead.")
    web_used = {p.get("webPort") for p in profiles()}
    web = next(value for value in range(8721, 8821) if value not in web_used)
    return {"id": key, "host": session.host, "route": session.route, "share": share["name"],
            "nasRoot": root, "createFolder": False, "localRoot": str(local), "join": True, "repair": repair,
            "base": managed["base"], "account": account, "port": port, "webPort": web,
            "namespace": namespace, "profile": str(profile), "mountPoint": "/mnt/ananas-" + key,
            "uid": os.getuid(), "gid": os.getgid(), "username": __import__('pwd').getpwuid(os.getuid()).pw_name}


def join(session, inventory, plan, helper, progress=lambda message: None):
    try:
        return _join(session, inventory, plan, helper, progress)
    except Exception as error:
        message = str(error) if isinstance(error, SetupError) else "A step could not complete while adding this PC. The NAS helper and the other PC were not reconfigured."
        record = Path(plan["profile"]) / "setup.json"
        if record.exists():
            message += " A partial setup was preserved for recovery at " + str(record) + ". Do not create a competing setup for this directory."
        raise SetupError(message) from None


def _join(session, inventory, plan, helper, progress):
    """Add this PC to an installed helper. The synchronized directory, the helper's
    own identity and every already-enrolled PC are left exactly as they are."""
    progress("Checking the NAS helper and the selected folder…")
    validate_local(plan['localRoot'])
    if session.route != lan(session.host):
        raise SetupError("The LAN connection changed. Reconnect and try again.")
    q = shlex.quote
    base, root, source = plan["base"], plan["nasRoot"], plan["route"]["source"]
    config = dict(helper["original"] if plan.get("repair") and helper.get("original") else helper["config"])
    if config.get("root") != root:
        raise SetupError("The installed helper no longer serves this directory. Refresh the folder list and retry.")
    if source in config.get("peers", []):
        raise SetupError("This PC is already allowed by that helper. It has not been changed.")
    profile = Path(plan["profile"])
    profile.mkdir(mode=0o700, parents=True)
    record = profile / "setup.json"
    atomically(record, json.dumps({"phase": "staging", "plan": plan}, indent=2).encode())
    local = Path(plan["localRoot"])
    local.mkdir(mode=0o700, parents=True, exist_ok=True)
    tls = profile / "tls"
    tls.mkdir(mode=0o700)
    # The helper verifies this PC against the issuer bundle it already trusts; this
    # PC verifies the helper with the issuer that signed the certificate it serves.
    private_write(tls / "ca.pem", helper["ca"])
    server_pin = hashlib.sha256(run(["openssl", "x509", "-outform", "DER"], data=helper["server"], timeout=20)).hexdigest()
    pc_config = {"local": {"root": str(local)}, "nas": {"host": session.host, "share": plan["share"],
                 "mountPoint": plan["mountPoint"], "protocol": "smb", "interface": session.route["interface"], "prefix": session.route["prefix"]},
                 "stateDir": str(Path.home() / ".local/state/nas-sync" / plan["id"]), "webPort": plan["webPort"], "lanGuard": True,
                 "selectiveSync": {"excludeLocal": [], "excludeRemote": ["/@Recycle/"]},
                 "limits": {"scanOpsPerSecond": 10000, "readBytesPerSecond": 1073741824, "cacheBytes": 1099511627776, "maxWatches": 100000}}
    config_path = profile / "config.json"
    atomically(config_path, json.dumps(pc_config).encode())
    binary = Path.home() / ".local/bin/nas-sync"
    identity = json.loads(run([str(binary), "-config", str(config_path), "-client-identity"]))["clientID"]
    pin = join_certificates(tls, source)
    if pin in config.get("clients", {}) or identity in config.get("clients", {}).values():
        raise SetupError("That helper already lists this PC's identity. Remove the stale entry on the NAS before retrying.")
    pc_config["sync"] = {"enabled": True, "port": plan["port"], "source": source,
                         "namespace": plan["namespace"], "replicaNamespace": secrets.token_hex(32),
                         "certificate": str(tls / "client.pem"), "privateKey": str(tls / "client.key"), "ca": str(tls / "ca.pem"),
                         "serverFingerprint": server_pin, "maxFileBytes": config.get("maxFileBytes", 8589934592),
                         "maxBatchBytes": config.get("maxBatchBytes", 8589934592), "maxCacheEntries": 1000000}
    owner = f"{plan['account']['uid']}:{plan['account']['gid']}"
    check = shlex.join(["/bin/busybox", "start-stop-daemon", "-S", "-c", owner, "-x", base + "/helper",
                        "--", "-config", base + "/helper.json", "-check-config"])
    # Everything this transaction depends on is exercised while the helper is still
    # untouched, so an unusable NAS stops the setup before anything is modified.
    progress("Checking what this NAS supports…")
    for program in ("sha256sum", "cp", "mv", "chown"):
        remote(session, f"looking for {program}", "command -v " + q(program), admin=True)
    remote(session, "checking the helper's firewall command", "test -x /sbin/iptables", admin=True)
    for path in (config["clientCA"], base + "/helper.json", base + "/service.sh"):
        remote(session, "checking " + Path(path).name, "set -eu; test -f " + q(path) + "; test -w " + q(path), admin=True)
    try:
        session.command(check, admin=True)
        verifiable = True
    except SetupError:
        verifiable = False
    progress("Adding this PC to the NAS helper's allowed list…")
    config["peers"] = list(config.get("peers", [])) + [source]
    config["clients"] = dict(config.get("clients", {}), **{pin: identity})
    # Two PCs each need their own connection and notification stream.
    config["maxConnections"] = max(int(config.get("maxConnections", 2)), min(8, 2 * len(config["peers"])))
    # Recorded before the first change, so a later failure never reports the NAS as untouched.
    atomically(record, json.dumps({"phase": "nas-updating", "plan": plan}, indent=2).encode())
    for path, data in ((config["clientCA"], None), (base + "/helper.json", json.dumps(config).encode())):
        backup = path + ".before-enroll"
        # Kept from the first attempt only, so a retry never overwrites the pristine
        # copy, and a retry's bundle is rebuilt from it rather than appended twice.
        remote(session, "saving the original " + Path(path).name,
               "set -eu; test -e " + q(backup) + " || cp -p " + q(path) + " " + q(backup), admin=True)
        if data is None:
            pristine = helper.get("originalCA") or remote(session, "reading the original " + Path(path).name,
                                                          "head -c 65536 " + q(backup), admin=True).encode()
            data = (pristine if pristine.endswith(b"\n") else pristine + b"\n") + (tls / "client-ca.pem").read_bytes()
        temporary = path + ".enroll-new"
        remote(session, "writing " + Path(path).name,
               "set -eu; umask 077; rm -f " + q(temporary) + "; cat > " + q(temporary) +
               "; chmod 600 " + q(temporary) + "; chown " + owner + " " + q(temporary) +
               "; mv " + q(temporary) + " " + q(path), admin=True, data=data, timeout=120)
        verified = remote(session, "verifying " + Path(path).name, "sha256sum " + q(path), admin=True)
        if verified.split()[0] != hashlib.sha256(data).hexdigest():
            raise SetupError("An updated helper file did not pass its integrity check. The helper was not restarted.")
    if verifiable:
        remote(session, "checking the updated helper configuration", check, admin=True)
    atomically(record, json.dumps({"phase": "nas-updated", "plan": plan}, indent=2).encode())
    progress("Preparing saved-credential mounting and PC autostart (administrator confirmation)…")
    system_plan = dict(plan, password=session.password, nasUsername=session.username)
    run(["pkexec", "/usr/bin/python3", str(bundle_home() / "ananas_setup_system.py"), "--install"],
        data=json.dumps(system_plan).encode(), timeout=180)
    atomically(record, json.dumps({"phase": "pc-prepared", "plan": plan}, indent=2).encode())
    progress("Allowing this PC through the NAS firewall and restarting the helper…")
    # Each chain the script already uses gains one rule for this PC, mirroring the
    # enrolled PC's own rule so inbound and outbound treatment stay identical. The
    # chains persist until the NAS reboots, so the running rule set is updated as
    # well as the startup script that rebuilds it.
    service = helper["originalService"] if plan.get("repair") and helper.get("originalService") else helper["service"]
    rules, added = firewall_rules(service, config.get("peers", [])), []
    if plan.get("repair"):
        rules = [rule for rule in rules if rule["peer"] != source]
    for rule in sorted(rules, key=lambda item: item["end"], reverse=True):
        text = re.sub(r"(?<![0-9.])" + re.escape(rule["peer"]) + r"(?![0-9.])", source, rule["text"])
        if text == rule["text"] or source not in text:
            raise SetupError("The installed helper's firewall rules could not be extended to this PC. They have not been changed.")
        if text in service:
            raise SetupError("The installed helper's startup script already allows this PC. It has not been changed.")
        arguments = text.split(" -A ", 1)[1]
        remote(session, "allowing this PC in chain " + rule["chain"],
               "set -eu; /sbin/iptables -C " + arguments + " 2>/dev/null || /sbin/iptables -I " +
               arguments.replace(rule["chain"], rule["chain"] + " 1", 1), admin=True)
        service = service[:rule["end"]] + "\n" + rule["indent"] + text + service[rule["end"]:]
        added.append(text)
    if not added:
        raise SetupError("The installed helper's startup script could not be updated. This PC was not enrolled.")
    script = base + "/service.sh"
    remote(session, "saving the original service.sh",
           "set -eu; test -e " + q(script + ".before-enroll") + " || cp -p " + q(script) + " " + q(script + ".before-enroll"), admin=True)
    remote(session, "writing service.sh",
           "set -eu; umask 077; rm -f " + q(script + ".enroll-new") + "; cat > " + q(script + ".enroll-new") +
           "; chmod 755 " + q(script + ".enroll-new") + "; chown 0:0 " + q(script + ".enroll-new") +
           "; mv " + q(script + ".enroll-new") + " " + q(script), admin=True, data=service.encode(), timeout=120)
    remote(session, "stopping the helper", q(script) + " stop || true", admin=True)
    time.sleep(1)
    try:
        session.command(q(script) + " start", admin=True)
        time.sleep(1)
        session.command(q(script) + " status", admin=True)
    except SetupError:
        # The already-enrolled PC must not lose its helper because this PC failed
        # to join: restore the saved originals and start the helper as it was.
        restored = []
        for path in (config["clientCA"], base + "/helper.json", script):
            try:
                session.command("set -eu; test -e " + q(path + ".before-enroll") + " && cp -p " +
                                q(path + ".before-enroll") + " " + q(path), admin=True)
                restored.append(Path(path).name)
            except SetupError:
                pass
        try:
            session.command(q(script) + " stop || true", admin=True)
            session.command(q(script) + " start", admin=True)
            session.command(q(script) + " status", admin=True)
        except SetupError:
            raise SetupError("The NAS helper did not restart after this PC was added, and could not be restored automatically. "
                             "Its original files are saved beside it with the suffix .before-enroll; restore them on the NAS and "
                             "run " + script + " start.") from None
        raise SetupError("The NAS helper did not restart after this PC was added, so its previous configuration was restored (" +
                         ", ".join(restored) + ") and it is running again. This PC was not enrolled.") from None
    atomically(config_path, json.dumps(pc_config, indent=2).encode())
    progress("Starting this folder's sync daemon…")
    if local.resolve() != local or any(local.iterdir()):
        raise SetupError('The local folder changed during setup or is no longer empty. Sync was not started; choose an empty folder before recovering this setup.')
    services = Path.home() / ".config/systemd/user"
    services.mkdir(parents=True, exist_ok=True)
    service_name = "ananas-sync-" + plan["id"] + ".service"
    private_write(services / service_name, service_text(binary, config_path).encode(), 0o644)
    run(["systemctl", "--user", "daemon-reload"])
    run(["systemctl", "--user", "enable", "--now", service_name])
    atomically(record, json.dumps({"phase": "started", "plan": plan}, indent=2).encode())
    return {"message": "This PC was added to the existing NAS folder. Its contents will download into " + str(local) +
                       ", and changes on either PC now synchronize through the NAS.",
            "profile": str(config_path), "service": service_name}


def install(session, inventory, plan, progress=lambda message: None):
    try:
        return _install(session, inventory, plan, progress)
    except Exception as error:
        message = str(error) if isinstance(error, SetupError) else "An installation step could not complete. Check the NAS connection and setup prerequisites."
        record = Path(plan["profile"]) / "setup.json"
        if record.exists():
            message += " A partial setup was preserved for recovery at " + str(record) + ". Existing sync instances were not replaced. Do not create a competing setup for this directory."
        raise SetupError(message) from None


def _install(session, inventory, plan, progress):
    """Stage a new independent instance; never replace any pre-existing helper."""
    progress("Checking the NAS and selected folder…")
    validate_local(plan['localRoot'])
    if session.route != lan(session.host):
        raise SetupError("The LAN connection changed. Reconnect and try again.")
    account, base, root = plan["account"], plan["base"], plan["nasRoot"]
    arch = {"aarch64": "arm64", "x86_64": "amd64", "armv7l": "arm"}.get(inventory["architecture"])
    if not arch or not (bundle_home() / "bin" / arch / "helper").is_file():
        raise SetupError("This QNAP CPU is not included in the installer. Supported: ARM64, ARMv7 and x86-64.")
    q = shlex.quote
    remote_interface = session.command("/sbin/ip -4 route get " + q(session.route["source"]), admin=True)
    match = re.search(r"\bdev ([A-Za-z0-9_.-]{1,15})\b", remote_interface)
    if not match or " via " in remote_interface:
        raise SetupError("The NAS does not have a direct return route to this PC.")
    device = match.group(1)
    session.command("set -eu; test ! -e " + q(base) + "; command -v sha256sum; test -x /bin/busybox; test -x /sbin/setcfg", admin=True)
    parent = str(Path(root).parent) if plan["createFolder"] else root
    canonical = session.command('cd ' + q(parent) + ' && pwd -P', admin=True).strip()
    if canonical != parent:
        raise SetupError('The selected NAS folder was moved or is a symbolic link. Refresh the folder list before setting up sync.')
    permission = "test -d " + q(parent) + " && test -w " + q(parent) + " && test -x " + q(parent)
    try:
        session.command(shlex.join(["/bin/busybox", "start-stop-daemon", "-S", "-c", f"{account['uid']}:{account['gid']}",
                                    "-x", "/bin/sh", "--", "-c", permission]), admin=True)
    except SetupError:
        raise SetupError("The selected non-admin account cannot write this NAS directory. Grant it read/write access in QTS or select another account.") from None
    if plan["createFolder"]:
        session.command("test ! -e " + q(root), admin=True)
    profile = Path(plan["profile"])
    profile.mkdir(mode=0o700, parents=True)
    record = profile / "setup.json"
    atomically(record, json.dumps({"phase": "staging", "plan": plan}, indent=2).encode())
    # Save enough state for diagnosis/recovery, but never save the NAS password.
    local = Path(plan["localRoot"])
    local.mkdir(mode=0o700, parents=True, exist_ok=True)
    tls = profile / "tls"
    tls.mkdir(mode=0o700)
    state = Path.home() / ".local/state/nas-sync" / plan["id"]
    config = {"local": {"root": str(local)}, "nas": {"host": session.host, "share": plan["share"],
              "mountPoint": plan["mountPoint"], "protocol": "smb", "interface": session.route["interface"], "prefix": session.route["prefix"]},
              "stateDir": str(state), "webPort": plan["webPort"], "lanGuard": True,
              "selectiveSync": {"excludeLocal": [], "excludeRemote": ["/@Recycle/"]},
              "limits": {"scanOpsPerSecond": 10000, "readBytesPerSecond": 1073741824, "cacheBytes": 1099511627776, "maxWatches": 100000}}
    config_path = profile / "config.json"
    atomically(config_path, json.dumps(config).encode())
    binary = Path.home() / ".local/bin/nas-sync"
    identity = json.loads(run([str(binary), "-config", str(config_path), "-client-identity"]))["clientID"]
    pins = certificates(tls, session.host, session.route["source"])
    namespace = secrets.token_hex(32)
    # No fixed "source": the daemon follows the PC's current DHCP address, and the
    # NAS accepts the whole direct subnet; the client certificate pin authenticates.
    config["sync"] = {"enabled": True, "port": plan["port"],
                      "namespace": namespace, "replicaNamespace": secrets.token_hex(32),
                      "certificate": str(tls / "client.pem"), "privateKey": str(tls / "client.key"), "ca": str(tls / "ca.pem"),
                      "serverFingerprint": pins["server"], "maxFileBytes": 8589934592, "maxBatchBytes": 8589934592, "maxCacheEntries": 1000000}
    nas = {"liveWrites": True, "root": root, "stateDir": base + "/state", "observerStateDir": base + "/observer",
           "namespace": namespace, "listen": f":{plan['port']}", "interface": device, "prefix": session.route["prefix"],
           "peers": [session.route["prefix"]], "uid": account["uid"], "gid": account["gid"], "groups": [account["gid"]],
           "certificate": base + "/server.pem", "privateKey": base + "/server.key", "clientCA": base + "/ca.pem",
           "clients": {pins["client"]: identity}, "exclusions": ["@Recycle/"], "maxConnections": 2,
           "maxFileBytes": 8589934592, "maxBatchBytes": 8589934592, "maxCacheBytes": 1099511627776,
           "maxCacheEntries": 1000000, "readBytesPerSecond": 1073741824}
    progress("Installing the QNAP helper and its private certificates…")
    session.command("set -eu; umask 077; mkdir " + q(base) + "; chmod 755 " + q(base) +
                    "; mkdir " + q(base + "/state") + " " + q(base + "/observer") +
                    "; chown " + f"{account['uid']}:{account['gid']} " + q(base + "/state") + " " + q(base + "/observer"), admin=True)
    if plan["createFolder"]:
        session.command("set -eu; mkdir " + q(root) + "; chown " + f"{account['uid']}:{account['gid']} " + q(root), admin=True)
    launch = shlex.join(["-helper", base + "/helper", "-config", base + "/helper.json", "-listen", nas["listen"],
                        "-interface", device, "-prefix", session.route["prefix"], "-peer", session.route["prefix"],
                        "-uid", str(account["uid"]), "-gid", str(account["gid"])])
    # Device-bound sockets and explicit peer filtering remain enforced by the
    # helper. Only this fresh instance's dedicated port receives firewall rules.
    chain = "AN" + plan["id"][:12]
    startup = f'''#!/bin/sh
set -eu
case "${{1:-}}" in
start)
 if ! /sbin/iptables -nL {chain} >/dev/null 2>&1; then
  /sbin/iptables -N {chain}
  /sbin/iptables -A {chain} -s {session.route['prefix']} -i {device} -j ACCEPT
  /sbin/iptables -A {chain} -j DROP
  /sbin/iptables -I INPUT 1 -p tcp --dport {plan['port']} -j {chain}
 fi
 /bin/busybox start-stop-daemon -S -b -m -p /var/run/ananas-{plan['id']}.pid -x {q(base + '/launcher')} -- {launch}
 ;;
stop) /bin/busybox start-stop-daemon -K -p /var/run/ananas-{plan['id']}.pid -x {q(base + '/launcher')} -s TERM ;;
status) /bin/busybox start-stop-daemon -K -t -p /var/run/ananas-{plan['id']}.pid -x {q(base + '/launcher')} ;;
*) exit 2;;
esac
'''
    payloads = {"helper.json": json.dumps(nas).encode(), "service.sh": startup.encode()}
    payloads.update({name: (tls / name).read_bytes() for name in ("server.pem", "server.key", "ca.pem")})
    payloads.update({name: (bundle_home() / "bin" / arch / name).read_bytes() for name in ("helper", "launcher")})
    for name, data in payloads.items():
        path = base + "/" + name
        executable = name in ("helper", "launcher", "service.sh")
        owner = "0:0" if executable else f"{account['uid']}:{account['gid']}"
        session.command("set -eu; umask 077; set -C; cat > " + q(path) + "; chmod " + ("755 " if executable else "600 ") +
                        q(path) + "; chown " + owner + " " + q(path), admin=True, data=data, timeout=120)
        if session.command("sha256sum " + q(path), admin=True).split()[0] != hashlib.sha256(data).hexdigest():
            raise SetupError("The uploaded helper did not pass its integrity check. Sync has not been enabled.")
    session.command(shlex.join(["/bin/busybox", "start-stop-daemon", "-S", "-c", f"{account['uid']}:{account['gid']}",
                                "-x", base + "/helper", "--", "-config", base + "/helper.json", "-check-config"]), admin=True)
    atomically(config_path, json.dumps(config, indent=2).encode())
    progress("Preparing saved-credential mounting and PC autostart (administrator confirmation)…")
    system_plan = dict(plan, password=session.password, nasUsername=session.username, pin=pins["server"])
    run(["pkexec", "/usr/bin/python3", str(bundle_home() / "ananas_setup_system.py"), "--install"],
        data=json.dumps(system_plan).encode(), timeout=180)
    atomically(record, json.dumps({"phase": "pc-prepared", "plan": plan}, indent=2).encode())
    for key, value in {"Name": "anaNAS-" + plan["id"], "Display_Name": "anaNAS " + plan["share"], "Version": "0.2.0",
                       "Enable": "TRUE", "Shell": base + "/service.sh", "Install_Path": base, "Author": "anaNAS", "Desktop": "0"}.items():
        session.command(shlex.join(["/sbin/setcfg", "anaNAS-" + plan["id"], key, value, "-f", "/etc/config/qpkg.conf"]), admin=True)
    session.command(q(base + "/service.sh") + " start", admin=True)
    time.sleep(1)
    session.command(q(base + "/service.sh") + " status", admin=True)
    progress("Starting this folder's sync daemon…")
    if local.resolve() != local or any(local.iterdir()):
        raise SetupError('The local folder changed during setup or is no longer empty. Sync was not started; choose an empty folder before recovering this setup.')
    services = Path.home() / ".config/systemd/user"
    services.mkdir(parents=True, exist_ok=True)
    service = "ananas-sync-" + plan["id"] + ".service"
    private_write(services / service, service_text(binary, config_path).encode(), 0o644)
    run(["systemctl", "--user", "daemon-reload"])
    run(["systemctl", "--user", "enable", "--now", service])
    # The panel follows the first configured profile, leaving an existing primary
    # profile byte-for-byte unchanged. Additional profiles are listed in setup.
    primary = config_home() / "config.json"
    if not primary.exists():
        private_write(primary, json.dumps(config, indent=2).encode())
        # Avoid starting a second process on the same index on the next boot.
        if (Path.home() / ".config/systemd/user/nas-sync.service").exists():
            run(["systemctl", "--user", "disable", "nas-sync.service"])
        run(["systemctl", "--user", "try-restart", "nas-sync-indicator.service"])
    atomically(record, json.dumps({"phase": "started", "plan": plan}, indent=2).encode())
    from nas_sync_indicator import Client
    deadline = time.monotonic() + 30
    last = "Daemon is starting"
    while time.monotonic() < deadline:
        try:
            status = Client(plan["webPort"]).status()
            last = status.get("syncReason", last)
            if status.get("automaticWrites") and status.get("ready") and "LAN sync active" in last:
                progress("Sync is active. Existing NAS files will appear in your local folder.")
                return {"active": True, "message": last, "profile": str(config_path)}
        except Exception:
            pass
        time.sleep(1)
    return {"active": False, "message": "Installed, but synchronization is not ready yet: " + last,
            "profile": str(config_path)}
