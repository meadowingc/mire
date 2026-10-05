"""Deploy a committed Mire release, optionally migrating the verified legacy tmux process."""

import argparse
from contextlib import closing
import copy
import fcntl
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import platform
import pwd
import re
import shlex
import shutil
import signal
import socket
import sqlite3
import subprocess
import sys
import tarfile
import tempfile
import time
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode, urlsplit
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener
import uuid


class DeploymentError(RuntimeError):
    pass


def require(condition, message):
    if not condition:
        raise DeploymentError(message)


def checksum(path):
    digest = hashlib.sha256()
    with Path(path).open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def private_write(path, contents):
    path = Path(path)
    descriptor, temporary = tempfile.mkstemp(prefix=".install-", dir=path.parent)
    temporary = Path(temporary)
    try:
        with os.fdopen(descriptor, "w") as output:
            output.write(contents)
            output.flush()
            os.fsync(output.fileno())
        temporary.replace(path)
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        temporary.unlink(missing_ok=True)


def extract(archive, destination):
    seen = set()
    with tarfile.open(archive) as source:
        for member in source:
            name = PurePosixPath(member.name)
            require(name.parts and not name.is_absolute() and ".." not in name.parts,
                    "Unsafe release member path")
            require(member.name not in seen and (member.isfile() or member.isdir()),
                    "Release contains duplicate paths, links or special files")
            seen.add(member.name)
            target = destination.joinpath(*name.parts)
            target.parent.mkdir(parents=True, exist_ok=True)
            if member.isdir():
                target.mkdir(exist_ok=True)
            else:
                with source.extractfile(member) as incoming, target.open("xb") as output:
                    shutil.copyfileobj(incoming, output)


def release_files(directory):
    paths = list(directory.rglob("*"))
    require(not directory.is_symlink() and not any(path.is_symlink() for path in paths),
            "Release must not contain symlinks")
    return {path.relative_to(directory).as_posix(): checksum(path)
            for path in paths if path.is_file() and path != directory / "release.json"}


def sync_directory(path):
    descriptor = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def database_digest(connection):
    require(connection.execute("PRAGMA integrity_check").fetchall() == [("ok",)],
            "Database integrity check failed")
    digest = hashlib.sha256()
    connection.text_factory = bytes
    try:
        schema = connection.execute(
            "SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY type,name"
        ).fetchall()
        digest.update(repr(schema).encode("ascii"))
        names = connection.execute(
            "SELECT name FROM sqlite_master WHERE type='table' ORDER BY name"
        ).fetchall()
        for (name,) in names:
            quoted = '"' + name.decode().replace('"', '""') + '"'
            columns = connection.execute(f"PRAGMA table_info({quoted})").fetchall()
            primary = sorted((column[5], column[1]) for column in columns if column[5])
            order = ",".join('"' + column.decode().replace('"', '""') + '"'
                             for _, column in primary) or "rowid"
            digest.update(name)
            for row in connection.execute(f"SELECT * FROM {quoted} ORDER BY {order}"):
                digest.update(repr(row).encode("ascii"))
        for pragma in ("user_version", "application_id"):
            digest.update(repr(connection.execute(f"PRAGMA {pragma}").fetchone()).encode("ascii"))
    finally:
        connection.text_factory = str
    return digest.hexdigest()


def read_digest(path):
    require(path.is_file() and not path.is_symlink(), "Database is missing or redirected")
    with closing(sqlite3.connect(path.resolve().as_uri() + "?mode=ro", uri=True)) as connection:
        connection.execute("BEGIN")
        return database_digest(connection)


def snapshot(source, destination):
    require(source.is_file() and not source.is_symlink(), "Source database is missing or redirected")
    descriptor = os.open(destination, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    os.close(descriptor)
    deadline = time.monotonic() + 120

    def progress(status, remaining, total):
        require(time.monotonic() < deadline, "Database snapshot exceeded its deadline")

    with closing(sqlite3.connect(source.resolve().as_uri() + "?mode=ro", uri=True)) as incoming:
        with closing(sqlite3.connect(destination)) as output:
            incoming.backup(output, pages=256, progress=progress, sleep=0.05)
            return database_digest(output)


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, message, headers, new_url):
        return None


def request(url, fields=None):
    data = None if fields is None else urlencode(fields).encode()
    outgoing = Request(url, data=data, headers={"User-Agent": "MireDeploymentCheck/1.0"})
    try:
        response = build_opener(ProxyHandler({}), NoRedirect()).open(outgoing, timeout=20)
    except HTTPError as error:
        response = error
    with response:
        body = response.read(4 * 1024 * 1024 + 1)
        require(len(body) <= 4 * 1024 * 1024, "Unexpectedly large application response")
        return response.status, response.headers, body


def check_http(origin, revision, legacy=False):
    path, expected = ("/api/v1/ping", b"pong") if legacy else ("/healthz", b"ok")
    code, headers, body = request(origin + path)
    challenge = " (Cloudflare challenge)" if headers.get("cf-mitigated") == "challenge" else ""
    require(code == 200 and body.strip() == expected, f"Health check failed: HTTP {code}{challenge}")
    if not legacy:
        require(headers.get("X-Mire-Revision") == revision, "Unexpected serving revision")
    for path in ("/", "/login", "/about", "/discover"):
        code, _, body = request(origin + path)
        require(code == 200 and b"<html" in body.lower(), f"Page check failed for {path}: HTTP {code}")
    code, _, _ = request(origin + "/login", {
        "username": "__mire_probe_" + uuid.uuid4().hex, "password": uuid.uuid4().hex,
    })
    require(code == 401, "Invalid credentials did not reach authentication")


def public_origin(value):
    parsed = urlsplit(value)
    require(parsed.scheme == "https" and parsed.hostname and not parsed.username
            and not parsed.password and parsed.path in ("", "/")
            and not parsed.query and not parsed.fragment, "Use a public HTTPS origin")
    return value.rstrip("/")


def process_identity(pid):
    process = Path("/proc") / str(pid)
    require(type(pid) is int and pid > 1 and process.exists(), "Target process does not exist")
    fields = (process / "stat").read_text().rsplit(") ", 1)[1].split()
    return {"pid": pid, "start": fields[19], "parent": int(fields[1]),
            "exe": os.readlink(process / "exe"),
            "cwd": os.readlink(process / "cwd"),
            "argv": (process / "cmdline").read_bytes().split(b"\0")[:-1]}


def stop_exact(identity, signum, timeout):
    current = process_identity(identity["pid"])
    require(current == identity, "Target process identity changed; refusing to signal it")
    os.kill(identity["pid"], signum)
    deadline = time.monotonic() + timeout
    while Path(f"/proc/{identity['pid']}").exists():
        try:
            fields = Path(f"/proc/{identity['pid']}/stat").read_text().rsplit(") ", 1)[1].split()
        except FileNotFoundError:
            return
        if fields[0] == "Z":
            return
        require(fields[19] == identity["start"], "Process ID was reused")
        require(time.monotonic() < deadline, "Target did not stop; no forced kill or data move performed")
        time.sleep(0.1)


def backup_configuration(config_text, unit_text):
    import tomllib
    original = tomllib.loads(config_text)
    expected = copy.deepcopy(original)
    selected = [entry for entry in expected["databases"] if entry.get("label") == "mire"]
    require(len(selected) == 1 and selected[0]["path"] == "/root/mire/mire.db",
            "Backuper's Mire source differs from the reviewed legacy path")
    selected[0]["path"] = "/var/lib/mire/mire.db"
    require(config_text.count('"/root/mire/mire.db"') == 1, "Ambiguous Backuper source")
    updated = config_text.replace('"/root/mire/mire.db"', '"/var/lib/mire/mire.db"', 1)
    require(not any(entry.get("label") == "mire-configuration"
                    or entry["path"] == "/etc/mire/mire.env" for entry in expected.get("files", [])),
            "Backuper already has a Mire configuration source; review coverage before migration")
    expected.setdefault("files", []).append({
        "path": "/etc/mire/mire.env", "label": "mire-configuration",
    })
    updated += '\n[[files]]\npath = "/etc/mire/mire.env"\nlabel = "mire-configuration"\n'
    require(tomllib.loads(updated) == expected, "Unexpected Backuper configuration change")
    lines = re.findall(r"(?m)^ReadWritePaths=(.*)$", unit_text)
    require(len(lines) == 1 and shlex.split(lines[0]).count("/root/mire") == 1,
            "Backuper's existing database-parent permission differs from the expected unit")
    require(unit_text.count('"/root/mire"') == 1, "Unexpected Backuper unit formatting")
    return updated, unit_text.replace('"/root/mire"', '"/var/lib/mire"', 1)


class Installer:
    def __init__(self, stage, system_root=Path("/")):
        self.stage = stage
        self.root = system_root / "opt/mire"
        self.current = self.root / "current"
        self.data = system_root / "var/lib/mire"
        self.database = self.data / "mire.db"
        self.environment = system_root / "etc/mire/mire.env"
        self.unit = system_root / "etc/systemd/system/mire.service"
        self.pending = self.root / "deployment-pending.json"
        self.backup_config = system_root / "etc/backuper/config.toml"
        self.backup_unit = system_root / "etc/systemd/system/backuper.service"
        self.legacy_database = system_root / "root/mire/mire.db"
        self.checks = system_root / "var/lib/mire-deploy-checks"

    def run(self, arguments, timeout=180, check=True):
        with (self.stage / "install.private.log").open("ab") as log:
            result = subprocess.run(arguments, stdout=subprocess.PIPE, stderr=log, timeout=timeout)
            if result.returncode and check:
                log.write(result.stdout)
                raise DeploymentError(f"{arguments[0]} failed; inspect the private deployment log")
        return result.stdout.decode().strip()

    def record(self, status, **fields):
        path = self.stage / "deployment.json"
        result = json.loads(path.read_text()) if path.exists() else {}
        result.update(status=status, **fields)
        private_write(path, json.dumps(result, indent=2) + "\n")

    def switch(self, release):
        candidate = self.root / (".current-" + uuid.uuid4().hex)
        candidate.symlink_to(release)
        candidate.replace(self.current)
        sync_directory(self.root)

    def require_recovery(self, message):
        self.run(["systemctl", "disable", "mire.service"], check=False)
        raise DeploymentError(message)

    def check_process(self, release):
        require(self.run(["systemctl", "is-active", "mire.service"]) == "active", "Mire is not active")
        pid = int(self.run(["systemctl", "show", "mire.service", "-p", "MainPID", "--value"]))
        require(Path(f"/proc/{pid}/exe").resolve() == release / "mire", "Unexpected running executable")
        require(Path(f"/proc/{pid}").stat().st_uid == self.user.pw_uid, "Mire is not using its dedicated user")

    def database_permissions(self):
        for suffix in ("", "-wal", "-shm"):
            path = Path(str(self.database) + suffix)
            if path.exists():
                require(path.is_file() and not path.is_symlink(), "Database/journal path was redirected")
                os.chown(path, self.user.pw_uid, self.user.pw_gid)
                path.chmod(0o600)

    def wait_health(self, origin, revision):
        deadline = time.monotonic() + 30
        while True:
            try:
                code, headers, body = request(origin + "/healthz")
                if code == 200 and body.strip() == b"ok" and headers.get("X-Mire-Revision") == revision:
                    return
            except (URLError, TimeoutError):
                pass
            require(time.monotonic() < deadline, "Mire did not become healthy within 30 seconds")
            time.sleep(0.25)

    def preflight(self, release, revision, source):
        parent = self.checks
        parent.mkdir(mode=0o711, exist_ok=True)
        parent.chmod(0o711)
        trial = Path(tempfile.mkdtemp(prefix="check-", dir=parent))
        name = "mire-check-" + uuid.uuid4().hex
        started = False
        try:
            before = snapshot(source, trial / "mire.db")
            shutil.copyfile(trial / "mire.db", self.stage / "preflight.db")
            os.chown(trial, self.user.pw_uid, self.user.pw_gid)
            os.chown(trial / "mire.db", self.user.pw_uid, self.user.pw_gid)
            with socket.socket() as listener:
                listener.bind(("127.0.0.1", 0))
                port = listener.getsockname()[1]
            origin = f"http://127.0.0.1:{port}"
            self.run([
                "systemd-run", "--quiet", "--collect", "--unit=" + name,
                "-p", "Type=exec", "-p", "User=mire", "-p", "Group=mire", "-p", "UMask=0077",
                "-p", "WorkingDirectory=" + str(trial), "-p", "NoNewPrivileges=true",
                "-p", "ProtectSystem=strict", "-p", "ProtectHome=true", "-p", "PrivateTmp=true",
                "-p", "ReadWritePaths=" + str(trial), "-p", "TimeoutStopSec=120",
                "-p", "IPAddressDeny=any", "-p", "IPAddressAllow=localhost",
                "-p", "StandardOutput=append:" + str(self.stage / "preflight.private.log"),
                "-p", "StandardError=append:" + str(self.stage / "preflight.private.log"),
                "--setenv=MIRE_LISTEN_ADDR=127.0.0.1:" + str(port),
                "--setenv=MIRE_DATABASE=" + str(trial / "mire.db"),
                "--setenv=MIRE_FILES_DIR=" + str(release / "files"),
                "--setenv=MIRE_REQUIRE_DATABASE=true", "--setenv=MIRE_BACKGROUND=false",
                str(release / "mire"),
            ])
            started = True
            self.wait_health(origin, revision)
            check_http(origin, revision)
            self.run(["systemctl", "stop", name])
            started = False
            require(read_digest(trial / "mire.db") == before,
                    "Rehearsal changed database contents/schema; review a migration separately")
            self.record("preflight_passed", preflight_digest=before)
        finally:
            if started:
                self.run(["systemctl", "stop", name])
            shutil.rmtree(trial)

    def activate(self, release, revision, metadata, legacy):
        initial = legacy is not None
        previous = None if initial else self.current.resolve()
        original_digest = None
        legacy_stopped = False
        before = {str(path): path.read_text() if path.exists() else None
                  for path in (self.environment, self.unit)}
        if initial:
            require(process_identity(legacy["app"]["pid"]) == legacy["app"]
                    and process_identity(legacy["wrapper"]["pid"]) == legacy["wrapper"],
                    "Legacy processes changed before cutover")
            before.update({str(path): path.read_text() for path in (self.backup_config, self.backup_unit)})
            backup_after = backup_configuration(before[str(self.backup_config)], before[str(self.backup_unit)])
        transaction = {"previous_release": str(previous) if previous else None,
                       "release": str(release), "initial": initial, "files_before": before}
        private_write(self.stage / "transaction.json", json.dumps(transaction, indent=2))
        private_write(self.pending, json.dumps({"receipt": str(self.stage), **transaction}, indent=2))
        changed_files = {}
        try:
            if initial:
                stop_exact(legacy["wrapper"], signal.SIGTERM, 15)
                # Stopping the wrapper reparents the child; pin its remaining identity again.
                identity = process_identity(legacy["app"]["pid"])
                require(all(identity[key] == legacy["app"][key] for key in ("pid", "start", "exe", "cwd", "argv")),
                        "Mire changed while its wrapper stopped")
                stop_exact(identity, signal.SIGTERM, 120)
                legacy_stopped = True
                original_digest = snapshot(self.legacy_database, self.stage / "before.db")
                require(not self.database.exists(), "Managed database appeared concurrently")
                shutil.copyfile(self.stage / "before.db", self.database)
                self.database.chmod(0o600)
                os.chown(self.database, self.user.pw_uid, self.user.pw_gid)
                with self.database.open("rb") as database:
                    os.fsync(database.fileno())
                sync_directory(self.data)
                require(read_digest(self.database) == original_digest, "Data relocation changed committed contents")
                changed_files[self.environment] = (
                    "MIRE_LISTEN_ADDR=127.0.0.1:5544\nMIRE_DATABASE=/var/lib/mire/mire.db\n"
                    "MIRE_FILES_DIR=/opt/mire/current/files\nMIRE_REQUIRE_DATABASE=true\nMIRE_BACKGROUND=true\n"
                )
                changed_files[self.backup_config], changed_files[self.backup_unit] = backup_after
            else:
                self.run(["systemctl", "stop", "mire.service"])
                original_digest = snapshot(self.database, self.stage / "before.db")
            self.record("backed_up", original_digest=original_digest)
            changed_files[self.unit] = (release / "mire.service").read_text()
            for path, contents in changed_files.items():
                require((path.read_text() if path.exists() else None) == before[str(path)],
                        "Configuration changed concurrently; refusing to overwrite it")
                private_write(path, contents)
                if path in (self.unit, self.backup_unit):
                    path.chmod(0o644)
            self.switch(release)
            self.database_permissions()
            self.run(["systemctl", "daemon-reload"])
            self.run(["systemd-analyze", "verify", str(self.unit)])
            self.run(["systemctl", "start", "mire.service"])
            self.wait_health("http://127.0.0.1:5544", revision)
            self.check_process(release)
            check_http(metadata["public_url"], revision)
            self.run(["systemctl", "enable", "mire.service"])
            self.record("healthy", release=str(release), revision=revision, data_relocation_verified=initial)
            private_write(self.root / "deployed.json", (self.stage / "deployment.json").read_text())
            self.pending.unlink()
        except BaseException:
            self.record("needs_recovery", original_digest=original_digest)
            self.run(["systemctl", "stop", "mire.service"], check=False)
            if initial and not legacy_stopped:
                raise DeploymentError("Legacy shutdown was not confirmed; inspect process state and the pending receipt") from None
            if original_digest is not None and self.database.exists():
                if read_digest(self.database) != original_digest:
                    self.require_recovery(
                        "Deployment failed after data changed. Mire is STOPPED and disabled; no database was restored. Inspect the receipt."
                    )
            for path, contents in changed_files.items():
                if (path.read_text() if path.exists() else None) not in (contents, before[str(path)]):
                    self.require_recovery("Operator configuration changed; preserve it and recover manually")
            for path in changed_files:
                old = before[str(path)]
                if old is None:
                    path.unlink(missing_ok=True)
                else:
                    private_write(path, old)
                    if path in (self.unit, self.backup_unit):
                        path.chmod(0o644)
            if previous:
                self.switch(previous)
            elif self.current.is_symlink():
                self.current.unlink()
            self.run(["systemctl", "daemon-reload"])
            if initial:
                require(original_digest is None or read_digest(self.legacy_database) == original_digest,
                        "Legacy database changed; refusing an automatic fallback")
                self.run([
                    "systemd-run", "--quiet", "--unit=mire-legacy-rollback", "-p", "Type=exec",
                    "-p", "WorkingDirectory=/root/mire", "-p", "Restart=on-failure",
                    "/root/mire/mire",
                ])
                deadline = time.monotonic() + 30
                while True:
                    try:
                        code, _, body = request("http://127.0.0.1:5544/api/v1/ping")
                        if code == 200 and body == b"pong":
                            break
                    except (URLError, TimeoutError):
                        pass
                    require(time.monotonic() < deadline, "Legacy fallback did not become healthy")
                    time.sleep(0.25)
                check_http(metadata["public_url"], "", legacy=True)
            else:
                self.database_permissions()
                self.run(["systemctl", "start", "mire.service"])
                self.check_process(previous)
            self.record("rolled_back")
            self.pending.unlink()
            raise

    def install(self):
        require(os.geteuid() == 0, "Run the remote installer as root")
        os.umask(0o077)
        self.root.mkdir(mode=0o755, exist_ok=True)
        self.root.chmod(0o755)
        with (self.root / ".deploy.lock").open("a") as deployment_lock:
            fcntl.flock(deployment_lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            require(not self.pending.exists(), "An interrupted deployment needs recovery; inspect deployment-pending.json")
            metadata = json.loads((self.stage / "request.json").read_text())
            metadata["public_url"] = public_origin(metadata["public_url"])
            require(checksum(self.stage / "release.tar") == metadata["archive_sha256"], "Release upload checksum mismatch")
            initial = metadata["migrate_tmux"]
            require(initial == (not self.current.exists()), "Migration/update mode does not match the live layout")
            legacy = None
            if initial:
                require(not self.environment.exists() and not self.unit.exists() and not self.database.exists()
                        and not self.current.is_symlink() and not (self.root / "deployed.json").exists(),
                        "An existing partial Mire installation needs manual review")
                app = process_identity(metadata["legacy_pid"])
                wrapper = process_identity(metadata["wrapper_pid"])
                require(app["exe"] == "/root/mire/mire" and app["cwd"] == "/root/mire"
                        and app["parent"] == wrapper["pid"]
                        and wrapper["cwd"] == "/root/mire"
                        and wrapper["argv"] == [b"bash", b"run_forever.sh"],
                        "Legacy process/wrapper differs from the reviewed deployment")
                require(checksum(Path(f"/proc/{app['pid']}/exe")) == metadata["legacy_sha256"],
                        "Legacy executable changed since approval")
                legacy = {"app": app, "wrapper": wrapper}
                source = self.legacy_database
                backup_configuration(self.backup_config.read_text(), self.backup_unit.read_text())
            else:
                require(self.current.is_symlink() and self.current.resolve().parent == self.root / "releases",
                        "Unrecognized current release")
                require(self.unit.read_bytes() == (self.current / "mire.service").read_bytes(),
                        "The installed unit has operator changes; review before replacing it")
                required = {"MIRE_LISTEN_ADDR=127.0.0.1:5544", "MIRE_DATABASE=/var/lib/mire/mire.db",
                            "MIRE_FILES_DIR=/opt/mire/current/files", "MIRE_REQUIRE_DATABASE=true", "MIRE_BACKGROUND=true"}
                require(required <= set(self.environment.read_text().splitlines()), "Unsupported managed runtime configuration")
                source = self.database
            require(shutil.disk_usage(self.root).free > source.stat().st_size * 5 + 512 * 1024**2,
                    "Insufficient space for release and database rehearsals")
            unpacked = self.stage / "unpacked"
            unpacked.mkdir()
            extract(self.stage / "release.tar", unpacked)
            manifest = json.loads((unpacked / "release.json").read_text())
            revision = manifest["revision"]
            require(re.fullmatch(r"[0-9a-f]{40}", revision), "Invalid committed revision")
            require(manifest["architecture"] == platform.machine(), "Release architecture differs from the VPS")
            files = release_files(unpacked)
            require(files == manifest["files"] and "mire" in files and "mire.service" in files,
                    "Release file manifest mismatch")
            require(all(name in ("mire", "mire.service") or name.startswith("files/") for name in files),
                    "Unexpected runtime payload")
            releases = self.root / "releases"
            releases.mkdir(mode=0o755, exist_ok=True)
            releases.chmod(0o755)
            release = releases / (revision + "-" + files["mire"][:12])
            if release.exists():
                require(release_files(release) == files, "An existing release differs from the upload")
            else:
                shutil.copytree(unpacked, release)
            for path in [release, *release.rglob("*")]:
                path.chmod(0o755 if path.is_dir() or path == release / "mire" else 0o644)
                if path.is_file():
                    with path.open("rb") as content:
                        os.fsync(content.fileno())
            for path in sorted([release, *(path for path in release.rglob("*") if path.is_dir())],
                               key=lambda path: len(path.parts), reverse=True):
                sync_directory(path)
            sync_directory(releases)
            try:
                self.user = pwd.getpwnam("mire")
            except KeyError:
                self.run(["useradd", "--system", "--user-group", "--no-create-home",
                          "--home-dir", "/var/lib/mire", "--shell", "/usr/sbin/nologin", "mire"])
                self.user = pwd.getpwnam("mire")
            require(self.user.pw_uid != 0 and self.user.pw_dir == "/var/lib/mire"
                    and self.user.pw_shell == "/usr/sbin/nologin", "Existing Mire account is not the expected service account")
            self.data.mkdir(mode=0o750, exist_ok=True)
            os.chown(self.data, self.user.pw_uid, self.user.pw_gid)
            self.environment.parent.mkdir(mode=0o750, exist_ok=True)
            check_http(metadata["public_url"], revision if initial else json.loads(
                (self.current / "release.json").read_text())["revision"], legacy=initial)
            if not initial and self.current.resolve() == release:
                self.check_process(release)
                self.record("already_current", release=str(release), revision=revision)
                return
            watched = (self.environment, self.unit, self.backup_config, self.backup_unit)
            expected_files = {path: path.read_bytes() if path.exists() else None for path in watched}
            self.preflight(release, revision, source)
            require(all((path.read_bytes() if path.exists() else None) == contents
                        for path, contents in expected_files.items()),
                    "Configuration changed during rehearsal; retry without overwriting operator changes")
            if initial:
                with Path("/var/lib/backuper/job.lock").open("a") as backup_lock:
                    fcntl.flock(backup_lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
                    self.activate(release, revision, metadata, legacy)
            else:
                self.activate(release, revision, metadata, legacy)


def ssh(host, arguments, capture=False):
    return subprocess.run(
        ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=15", host, shlex.join(arguments)],
        check=True, text=True, stdout=subprocess.PIPE if capture else None,
    )


def deploy(args):
    require(re.fullmatch(r"[A-Za-z0-9_][A-Za-z0-9_.@-]*", args.host), "Use a valid SSH host alias")
    url = public_origin(args.public_url)
    require(not args.migrate_tmux or (
        args.legacy_pid and args.wrapper_pid and args.legacy_sha256
        and re.fullmatch("[0-9a-f]{64}", args.legacy_sha256)
    ), "Initial migration requires the verified app PID, wrapper PID and executable SHA256")
    repository = Path(__file__).resolve().parents[1]

    def git(*arguments):
        return subprocess.check_output(["git", "-C", str(repository), *arguments], text=True).strip()

    require(not git("status", "--porcelain"), "Commit intended changes before deployment")
    revision = git("rev-parse", "HEAD")
    if not args.yes:
        require(input(f"Deploy {revision[:12]} to {args.host}, with a brief Mire stop? [y/N] ").lower() == "y",
                "Deployment cancelled")
    with tempfile.TemporaryDirectory(prefix="mire-deploy-") as temporary:
        directory = Path(temporary)
        source = directory / "source"
        source.mkdir()
        subprocess.run(["git", "-C", str(repository), "archive", "--format=tar",
                        "-o", str(directory / "source.tar"), revision], check=True)
        extract(directory / "source.tar", source)
        for tags in ("", "release"):
            subprocess.run(["go", "test", "-mod=readonly", "-tags", tags, "-count=1", "-timeout", "3m", "./..."],
                           cwd=source, check=True)
        subprocess.run([sys.executable, "-B", "-m", "unittest", "discover", "-s", "scripts", "-p", "test_deploy*.py"],
                       cwd=source, check=True)
        require(git("rev-parse", "HEAD") == revision and not git("status", "--porcelain"),
                "Source changed during validation")
        architecture = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine())
        require(platform.system() == "Linux" and architecture, "Build on Linux amd64 or arm64")
        payload = directory / "payload"
        payload.mkdir()
        subprocess.run(["go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-tags", "release",
                        "-ldflags", "-X main.buildRevision=" + revision, "-o", str(payload / "mire"), "."],
                       cwd=source, env={**os.environ, "GOOS": "linux", "GOARCH": architecture, "CGO_ENABLED": "0"}, check=True)
        shutil.copytree(source / "files", payload / "files")
        shutil.copyfile(source / "deploy/mire.service", payload / "mire.service")
        manifest = {"revision": revision, "architecture": platform.machine(), "files": release_files(payload)}
        (payload / "release.json").write_text(json.dumps(manifest, indent=2) + "\n")
        archive = directory / "release.tar"
        with tarfile.open(archive, "w") as output:
            for child in sorted(payload.iterdir()):
                output.add(child, arcname=child.name)
        metadata = {
            "public_url": url, "archive_sha256": checksum(archive), "migrate_tmux": args.migrate_tmux,
            "legacy_pid": args.legacy_pid, "wrapper_pid": args.wrapper_pid, "legacy_sha256": args.legacy_sha256,
        }
        (directory / "request.json").write_text(json.dumps(metadata))
        identifier = uuid.uuid4().hex
        stage = "/root/mire-deploy-backups/update-" + identifier
        ssh(args.host, ["install", "-d", "-m", "0700", stage])
        print("Private deployment receipt: " + stage, flush=True)
        subprocess.run(["scp", "-q", str(archive), str(directory / "request.json"),
                        str(directory / "source.tar"), str(source / "scripts/deploy_vps.py"),
                        args.host + ":" + stage + "/"], check=True)
        ssh(args.host, [
            "systemd-run", "--quiet", "--wait", "--collect", "--unit=mire-deploy-" + identifier,
            "-p", "Type=oneshot", "-p", "TimeoutStartSec=infinity", "-p", "UMask=0077",
            "-p", "StandardOutput=append:" + stage + "/install.private.log",
            "-p", "StandardError=append:" + stage + "/install.private.log",
            "/usr/bin/python3", "-B", stage + "/deploy_vps.py", "--install", stage,
        ])
        print("Mire deployment completed; inspect " + stage + "/deployment.json", flush=True)


def main():
    if sys.argv[1:2] == ["--install"]:
        require(len(sys.argv) == 3 and re.fullmatch(
            r"/root/mire-deploy-backups/update-[0-9a-f]{32}", sys.argv[2]), "Invalid installer receipt")
        stage = Path(sys.argv[2])
        installer = Installer(stage)
        try:
            installer.install()
        except BaseException:
            path = stage / "deployment.json"
            if not path.exists() or json.loads(path.read_text()).get("status") not in {"needs_recovery", "rolled_back"}:
                installer.record("failed")
            raise
        return
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("host", nargs="?", default="meadow-ubuntu-8gb-hel1-1")
    parser.add_argument("--public-url", default="https://mire.meadow.cafe")
    parser.add_argument("--yes", action="store_true")
    parser.add_argument("--migrate-tmux", action="store_true")
    parser.add_argument("--legacy-pid", type=int)
    parser.add_argument("--wrapper-pid", type=int)
    parser.add_argument("--legacy-sha256")
    deploy(parser.parse_args())


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        detail = str(error) if isinstance(error, DeploymentError) else type(error).__name__
        print("Deployment failed: " + detail + "; inspect the private receipt before retrying", file=sys.stderr)
        raise SystemExit(1)
