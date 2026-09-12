#!/usr/bin/python3
"""Install the specifically authorized Nasdir LAN trial; never reads NAS passwords.

Prepare with no --install to check paths/session/binaries. Installation creates
only new private NAS service/state paths, provisions fresh TLS identities, enables
the NAS helper and atomically updates the existing PC daemon/profile. Existing
share contents and owners are untouched. Failures preserve state for recovery.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import secrets
import shlex
import shutil
import subprocess
import tempfile
import time

from run_native_probe import binary_payload, remote, session_options

BASE = "/share/EXAMPLE_VOLUME/.ananas-service"
STATE = "/share/EXAMPLE_VOLUME/.ananas-nasdir-state"
OBSERVER = "/share/EXAMPLE_VOLUME/.ananas-observer-state"
ROOT = "/share/EXAMPLE_VOLUME/Nasdir"


def write_new(path, data, mode=0o600):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, mode)
    with os.fdopen(fd, "wb") as stream:
        stream.write(data)
        stream.flush()
        os.fsync(stream.fileno())


def provision_certs(directory):
    def openssl(*args):
        return subprocess.check_output(["openssl", *args], cwd=directory, stderr=subprocess.PIPE, timeout=15)
    openssl("req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256", "-nodes", "-keyout", "ca.key", "-out", "ca.pem", "-subj", "/CN=anaNAS-Nasdir-CA", "-days", "3650")
    for name, usage, address in (("server", "serverAuth", "10.23.42.30"), ("client", "clientAuth", "10.23.42.17")):
        openssl("req", "-new", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256", "-nodes", "-keyout", name+".key", "-out", name+".csr", "-subj", "/CN=anaNAS-Nasdir-"+name)
        (directory / (name+".ext")).write_text("subjectAltName=IP:"+address+"\nextendedKeyUsage="+usage+"\nkeyUsage=digitalSignature\n")
        openssl("x509", "-req", "-in", name+".csr", "-CA", "ca.pem", "-CAkey", "ca.key", "-CAcreateserial", "-out", name+".pem", "-days", "365", "-extfile", name+".ext")
    return {name: hashlib.sha256(openssl("x509", "-in", name+".pem", "-outform", "DER")).hexdigest() for name in ("server", "client")}


def run(args):
    if os.getuid() != 1000:
        raise ValueError("This fixed installation requires PC UID 1000")
    ssh = session_options(args.session_dir)
    helper, launcher = binary_payload(args.helper), binary_payload(args.launcher)
    pc = args.pc.resolve(strict=True)
    if not pc.is_file() or not os.access(pc, os.X_OK) or pc.stat().st_size > 32*1024*1024:
        raise ValueError("Built PC executable required")
    repo = Path(__file__).resolve().parent.parent
    home = Path.home()
    config = home / ".config/nas-sync/config.json"
    old = config.read_bytes()
    profile = json.loads(old)
    if profile["local"]["root"] != str(home / "NASdir") or profile["nas"] != {"host":"10.23.42.30","interface":"enp1s0","prefix":"10.23.42.0/24","share":"Nasdir","mountPoint":"/mnt/nasdir","protocol":"smb"}:
        raise ValueError("Existing daemon must target the exact dedicated Nasdir share")
    tls = config.parent / "tls"
    if tls.exists() or profile.get("sync", {}).get("enabled"):
        raise ValueError("Live installation already exists; preserve it and use an explicit update")
    check = remote(ssh, "set -eu; test ! -e "+BASE+"; test ! -e "+STATE+"; test ! -e "+OBSERVER+"; test -d "+ROOT+"; test \"$(/sbin/getcfg anaNAS Name -d ABSENT -f /etc/config/qpkg.conf)\" = ABSENT; id -u nas-sync-test; id -g nas-sync-test")
    if check.splitlines() != ["1000", "100"]:
        raise ValueError("NAS runtime identity differs")
    print(json.dumps({"phase":"prepared","localRoot":profile["local"]["root"],"nasRoot":ROOT,"newService":BASE,"newState":STATE,"helperBytes":len(helper),"launcherBytes":len(launcher),"install":args.install}),flush=True)
    if not args.install:
        return
    subprocess.run(["systemctl","--user","stop","nas-sync.service"],check=True,timeout=15)
    try:
        identity = json.loads(subprocess.check_output([str(pc),"-config",str(config),"-client-identity"],stderr=subprocess.PIPE,timeout=10))["clientID"]
        namespace, replica_namespace = secrets.token_hex(32), secrets.token_hex(32)
        with tempfile.TemporaryDirectory(prefix="ananas-live-identity-") as temporary:
            directory = Path(temporary)
            pins = provision_certs(directory)
            max_file, max_batch = 64*1024*1024, 128*1024*1024
            nas = {"liveWrites":True,"root":ROOT,"stateDir":STATE,"namespace":namespace,"listen":"10.23.42.30:8742","interface":"eth0","prefix":"10.23.42.0/24","peers":["10.23.42.17"],"uid":1000,"gid":100,"groups":[100],"certificate":BASE+"/server.pem","privateKey":BASE+"/server.key","clientCA":BASE+"/ca.pem","clients":{pins["client"]:identity},"exclusions":["@Recycle/"],"maxConnections":2,"maxFileBytes":max_file,"maxBatchBytes":max_batch,"maxCacheBytes":1024*1024*1024*1024,"maxCacheEntries":1000000,"readBytesPerSecond":1024*1024*1024}
            nas["observerStateDir"] = OBSERVER
            remote(ssh,"set -eu; umask 077; mkdir "+BASE+"; chmod 0755 "+BASE+"; mkdir "+STATE+" "+OBSERVER+"; chown 1000:100 "+STATE+" "+OBSERVER)
            payloads = {"helper":helper,"launcher":launcher,"service.sh":(repo/"deploy/ananas-qnap.sh").read_bytes(),"helper.json":(json.dumps(nas,indent=2)+"\n").encode()}
            payloads.update({name:(directory/name).read_bytes() for name in ("server.pem","server.key","ca.pem")})
            for name,data in payloads.items():
                path = BASE+"/"+name
                executable = name in ("helper","launcher","service.sh")
                mode = "0755" if executable else "0600"
                remote(ssh,"set -eu; umask 077; set -C; cat > "+shlex.quote(path)+"; chmod "+mode+" "+shlex.quote(path),data=data)
                if not executable:
                    remote(ssh,"chown 1000:100 "+shlex.quote(path))
                if remote(ssh,"sha256sum "+shlex.quote(path)).split()[0] != hashlib.sha256(data).hexdigest():
                    raise ValueError("Uploaded setup file differs")
            preflight = remote(ssh,shlex.join(["/bin/busybox","start-stop-daemon","-S","-c","nas-sync-test:everyone","-x",BASE+"/helper","--","-config",BASE+"/helper.json","-check-config"]))
            print(json.dumps({"phase":"native-preflight","result":json.loads(preflight)}),flush=True)
            os.mkdir(tls,0o700)
            for name in ("client.pem","client.key","ca.pem","ca.key"):
                write_new(tls/name,(directory/name).read_bytes())
            profile["sync"] = {"enabled":True,"port":8742,"source":"10.23.42.17","namespace":namespace,"replicaNamespace":replica_namespace,"certificate":str(tls/"client.pem"),"privateKey":str(tls/"client.key"),"ca":str(tls/"ca.pem"),"serverFingerprint":pins["server"],"maxFileBytes":max_file,"maxBatchBytes":max_batch,"maxCacheEntries":1000000}
            stamp = time.strftime("%Y%m%d-%H%M%S")
            write_new(config.with_name("config.before-live-"+stamp+".json"),old)
            staged = config.with_name("config.live-"+stamp+".json")
            write_new(staged,(json.dumps(profile,indent=2)+"\n").encode())
            target = home/".local/bin/nas-sync"
            backup = target.with_name("nas-sync.before-live-"+stamp)
            shutil.copy2(target,backup)
            candidate = target.with_name("nas-sync.live-"+stamp)
            write_new(candidate,pc.read_bytes(),0o755)
            # Persist startup configuration only for this new application section.
            fields = {"Name":"anaNAS","Display_Name":"anaNAS","Version":"0.1.0-trial","Enable":"TRUE","Shell":BASE+"/service.sh","Install_Path":BASE,"Author":"local","Desktop":"0"}
            for key,value in fields.items():
                remote(ssh,shlex.join(["/sbin/setcfg","anaNAS",key,value,"-f","/etc/config/qpkg.conf"]))
            remote(ssh,BASE+"/service.sh start")
            remote(ssh,BASE+"/service.sh status")
            print(json.dumps({"phase":"nas-ready","stagedPCConfig":str(staged),"stagedPCBinary":str(candidate)}),flush=True)
            # NAS provisioning is independent of PC administrator authentication.
            # The old observer remains installed if this final activation gate fails.
            subprocess.run(["systemctl","is-active","--quiet","ananas-network.service"],check=True,timeout=5)
            os.replace(candidate,target)
            os.replace(staged,config)
            print(json.dumps({"phase":"installed","clientID":identity,"namespace":namespace,"backupConfig":str(config.with_name("config.before-live-"+stamp+".json")),"backupBinary":str(backup),"nasRoot":ROOT,"pcConfig":str(config)}),flush=True)
    finally:
        subprocess.run(["systemctl","--user","start","nas-sync.service"],check=True,timeout=15)


if __name__ == "__main__":
    from legacy_profile import require_configured
    require_configured()
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--session-dir",type=Path,required=True)
    parser.add_argument("--pc",type=Path,required=True)
    parser.add_argument("--helper",type=Path,required=True)
    parser.add_argument("--launcher",type=Path,required=True)
    parser.add_argument("--install",action="store_true")
    run(parser.parse_args())
