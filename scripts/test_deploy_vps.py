from contextlib import closing
import copy
import io
import json
import os
from pathlib import Path
import signal
import sqlite3
import tarfile
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import deploy_vps as deploy


def create_database(path):
    with closing(sqlite3.connect(path)) as connection:
        connection.executescript(
            "CREATE TABLE entries(id INTEGER PRIMARY KEY AUTOINCREMENT, value TEXT);"
            "INSERT INTO entries(value) VALUES (CAST(X'80FF00' AS TEXT));"
        )


class DatabaseSafetyTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        self.source = self.root / "source.db"
        create_database(self.source)

    def test_snapshot_preserves_committed_wal_and_non_utf8_text(self):
        with closing(sqlite3.connect(self.source)) as writer:
            writer.execute("PRAGMA journal_mode=WAL")
            writer.execute("INSERT INTO entries(value) VALUES ('committed WAL')")
            writer.commit()
            target = self.root / "snapshot.db"
            self.assertEqual(deploy.snapshot(self.source, target), deploy.read_digest(self.source))
            self.assertEqual(deploy.read_digest(target), deploy.read_digest(self.source))
            self.assertEqual(target.stat().st_mode & 0o777, 0o600)

    def test_existing_destination_is_never_overwritten(self):
        target = self.root / "preserved.db"
        target.write_bytes(b"preserve")
        with self.assertRaises(FileExistsError):
            deploy.snapshot(self.source, target)
        self.assertEqual(target.read_bytes(), b"preserve")

    def test_missing_database_is_not_created(self):
        missing = self.root / "absent.db"
        with self.assertRaises(deploy.DeploymentError):
            deploy.snapshot(missing, self.root / "snapshot.db")
        self.assertFalse(missing.exists())

    def test_symlink_database_is_rejected(self):
        link = self.root / "link.db"
        link.symlink_to(self.source)
        with self.assertRaises(deploy.DeploymentError):
            deploy.snapshot(link, self.root / "snapshot.db")

    def test_data_and_schema_changes_change_digest(self):
        previous = deploy.read_digest(self.source)
        with closing(sqlite3.connect(self.source)) as connection:
            connection.execute("INSERT INTO entries(value) VALUES ('new')")
            connection.commit()
        updated = deploy.read_digest(self.source)
        self.assertNotEqual(previous, updated)
        with closing(sqlite3.connect(self.source)) as connection:
            connection.execute("ALTER TABLE entries ADD COLUMN extra TEXT")
        self.assertNotEqual(updated, deploy.read_digest(self.source))

    def test_archive_traversal_links_and_duplicates_are_rejected(self):
        for names, link in ((["../escape"], False), (["/absolute"], False),
                            (["link"], True), (["same", "same"], False)):
            with self.subTest(names=names, link=link), tempfile.TemporaryDirectory() as folder:
                folder = Path(folder)
                archive = folder / "source.tar"
                with tarfile.open(archive, "w") as output:
                    for name in names:
                        member = tarfile.TarInfo(name)
                        if link:
                            member.type = tarfile.SYMTYPE
                            member.linkname = "/outside"
                            output.addfile(member)
                        else:
                            member.size = 1
                            output.addfile(member, io.BytesIO(b"x"))
                with self.assertRaises(deploy.DeploymentError):
                    deploy.extract(archive, folder / "target")

    def test_nested_release_json_is_still_checksummed(self):
        (self.root / "files").mkdir()
        (self.root / "files/release.json").write_text("runtime asset")
        (self.root / "release.json").write_text("root manifest")
        files = deploy.release_files(self.root)
        self.assertIn("files/release.json", files)
        self.assertNotIn("release.json", files)


BACKUP_CONFIG = '''[settings]
keep_last = 10
[[databases]]
label = "mire"
path = "/root/mire/mire.db"
[[databases]]
label = "other"
path = "/var/lib/other/data.db"
[[destinations.rclone]]
remote = "preserve:first"
keep_last = 2
drive_use_trash = false
[[destinations.rclone]]
remote = "preserve:second"
keep_last = 1
'''
BACKUP_UNIT = '[Service]\nReadWritePaths="/root/mire" "/var/lib/backuper" "/var/lib/other"\n'


class BackupCoverageTests(unittest.TestCase):
    def test_only_mire_source_and_parent_permission_change(self):
        config, unit = deploy.backup_configuration(BACKUP_CONFIG, BACKUP_UNIT)
        self.assertEqual(config, BACKUP_CONFIG.replace('"/root/mire/mire.db"', '"/var/lib/mire/mire.db"')
                         + '\n[[files]]\npath = "/etc/mire/mire.env"\nlabel = "mire-configuration"\n')
        self.assertEqual(unit, BACKUP_UNIT.replace('"/root/mire"', '"/var/lib/mire"'))

    def test_unexpected_source_and_duplicate_parent_fail_closed(self):
        for config, unit in (
            (BACKUP_CONFIG.replace("/root/mire/mire.db", "/different/data.db"), BACKUP_UNIT),
            (BACKUP_CONFIG, BACKUP_UNIT.replace('"/root/mire"', '"/different"')),
            (BACKUP_CONFIG, BACKUP_UNIT.replace('"/root/mire"', '"/root/mire" "/root/mire"')),
            (BACKUP_CONFIG + '\n[[files]]\npath="/etc/mire/mire.env"\nlabel="existing"\n', BACKUP_UNIT),
        ):
            with self.subTest(config=config, unit=unit), self.assertRaises(deploy.DeploymentError):
                deploy.backup_configuration(config, unit)


class ActivationTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        stage = self.root / "receipt"
        stage.mkdir()
        self.installer = deploy.Installer(stage, self.root)
        self.installer.user = SimpleNamespace(pw_uid=os.getuid(), pw_gid=os.getgid())
        self.previous = self.installer.root / "releases/previous"
        self.release = self.installer.root / "releases/next"
        self.previous.mkdir(parents=True)
        self.release.mkdir()
        self.installer.current.symlink_to(self.previous)
        self.installer.data.mkdir(parents=True)
        self.installer.environment.parent.mkdir(parents=True)
        self.installer.unit.parent.mkdir(parents=True)
        self.installer.environment.write_text("MIRE_DATABASE=/var/lib/mire/mire.db\n")
        self.installer.unit.write_text("old unit\n")
        (self.release / "mire.service").write_text("new unit\n")
        create_database(self.installer.database)
        self.original = deploy.read_digest(self.installer.database)
        self.operations = []
        self.on_start = lambda: None
        self.metadata = {"public_url": "https://mire.example.test"}
        self.enterContext(patch.object(self.installer, "run", side_effect=self.fake_run))
        self.enterContext(patch.object(self.installer, "wait_health"))
        self.enterContext(patch.object(self.installer, "check_process"))
        self.http = self.enterContext(patch("deploy_vps.check_http"))

    def fake_run(self, command, **kwargs):
        if command[:2] in (["systemctl", "stop"], ["systemctl", "start"]):
            self.operations.append(command[1])
            if command[1] == "start":
                self.on_start()
        return ""

    def activate(self, legacy=None):
        self.installer.activate(self.release, "a" * 40, self.metadata, legacy)

    def receipt(self):
        return json.loads((self.installer.stage / "deployment.json").read_text())

    def fail_start(self, mutation=None):
        def failure():
            self.on_start = lambda: None
            if mutation:
                with closing(sqlite3.connect(self.installer.database)) as connection:
                    connection.executescript(mutation)
            raise deploy.DeploymentError("synthetic start failure")
        self.on_start = failure

    def test_success_preserves_database_and_environment(self):
        before = self.installer.environment.read_bytes()
        self.activate()
        self.assertEqual(self.operations, ["stop", "start"])
        self.assertEqual(self.installer.current.resolve(), self.release)
        self.assertEqual(deploy.read_digest(self.installer.database), self.original)
        self.assertEqual(deploy.read_digest(self.installer.stage / "before.db"), self.original)
        self.assertEqual(self.installer.environment.read_bytes(), before)
        self.assertEqual(self.installer.unit.stat().st_mode & 0o777, 0o644)
        self.assertEqual(self.receipt()["status"], "healthy")
        self.assertFalse(self.installer.pending.exists())

    def test_unchanged_data_allows_binary_only_rollback(self):
        self.fail_start()
        with self.assertRaisesRegex(deploy.DeploymentError, "synthetic"):
            self.activate()
        self.assertEqual(self.operations, ["stop", "start", "stop", "start"])
        self.assertEqual(self.installer.current.resolve(), self.previous)
        self.assertEqual(deploy.read_digest(self.installer.database), self.original)
        self.assertEqual(self.installer.unit.read_text(), "old unit\n")
        self.assertEqual(self.receipt()["status"], "rolled_back")

    def assert_changed_data_prevents_rollback(self, mutation):
        self.fail_start(mutation)
        with self.assertRaisesRegex(deploy.DeploymentError, "data changed"):
            self.activate()
        self.assertEqual(self.operations[-1], "stop")
        self.assertEqual(self.installer.current.resolve(), self.release)
        self.assertTrue(self.installer.pending.exists())
        self.assertEqual(self.receipt()["status"], "needs_recovery")
        self.assertEqual(deploy.read_digest(self.installer.stage / "before.db"), self.original)

    def test_new_writes_prevent_automatic_downgrade(self):
        self.assert_changed_data_prevents_rollback("INSERT INTO entries(value) VALUES ('accepted write')")

    def test_schema_changes_prevent_automatic_downgrade(self):
        self.assert_changed_data_prevents_rollback("ALTER TABLE entries ADD COLUMN version_two TEXT")

    def test_failed_snapshot_does_not_restore_or_replace_database(self):
        with patch("deploy_vps.snapshot", side_effect=sqlite3.OperationalError("snapshot failed")):
            with self.assertRaisesRegex(sqlite3.OperationalError, "snapshot failed"):
                self.activate()
        self.assertEqual(self.operations, ["stop", "stop", "start"])
        self.assertEqual(self.installer.current.resolve(), self.previous)
        self.assertEqual(deploy.read_digest(self.installer.database), self.original)

    def test_operator_unit_changes_are_not_overwritten_on_failure(self):
        def changed(*args):
            self.installer.unit.write_text("operator change\n")
            raise deploy.DeploymentError("health failed")
        self.http.side_effect = changed
        with self.assertRaisesRegex(deploy.DeploymentError, "Operator configuration"):
            self.activate()
        self.assertEqual(self.installer.unit.read_text(), "operator change\n")
        self.assertEqual(self.receipt()["status"], "needs_recovery")

    def test_initial_relocation_updates_backups_without_changing_old_data(self):
        self.installer.current.unlink()
        self.installer.unit.unlink()
        self.installer.environment.unlink()
        self.installer.legacy_database.parent.mkdir(parents=True)
        self.installer.database.replace(self.installer.legacy_database)
        self.installer.backup_config.parent.mkdir(parents=True)
        self.installer.backup_config.write_text(BACKUP_CONFIG)
        self.installer.backup_unit.write_text(BACKUP_UNIT)
        app = {"pid": 123, "start": "10", "exe": "/root/mire/mire", "cwd": "/root/mire",
               "argv": [b"./mire"], "parent": 456}
        wrapper = {"pid": 456}
        with patch("deploy_vps.process_identity", side_effect=lambda pid: copy.deepcopy(app if pid == 123 else wrapper)), \
                patch("deploy_vps.stop_exact") as stop:
            self.activate({"app": app, "wrapper": wrapper})
        self.assertEqual(stop.call_args_list[0].args[0], wrapper)
        self.assertEqual(stop.call_args_list[1].args[0], app)
        self.assertEqual(deploy.read_digest(self.installer.database), self.original)
        self.assertEqual(deploy.read_digest(self.installer.legacy_database), self.original)
        self.assertIn('path = "/var/lib/mire/mire.db"', self.installer.backup_config.read_text())
        self.assertIn('path = "/etc/mire/mire.env"', self.installer.backup_config.read_text())
        self.assertIn('"/var/lib/mire"', self.installer.backup_unit.read_text())
        self.assertTrue(self.receipt()["data_relocation_verified"])

    def test_changed_process_is_not_signalled(self):
        identity = {"pid": 123, "start": "previous"}
        with patch("deploy_vps.process_identity", return_value={"pid": 123, "start": "new"}), \
                patch("deploy_vps.os.kill") as kill:
            with self.assertRaisesRegex(deploy.DeploymentError, "identity changed"):
                deploy.stop_exact(identity, signal.SIGTERM, 1)
            kill.assert_not_called()

    def test_preflight_is_isolated_and_preserves_full_database(self):
        commands = []
        with patch.object(self.installer, "run", side_effect=lambda args, **kwargs: commands.append(args)):
            self.installer.preflight(self.release, "a" * 40, self.installer.database)
        launch = next(command for command in commands if command[0] == "systemd-run")
        for option in ("IPAddressDeny=any", "IPAddressAllow=localhost", "ProtectSystem=strict",
                       "ProtectHome=true", "User=mire", "--setenv=MIRE_BACKGROUND=false",
                       "--setenv=MIRE_REQUIRE_DATABASE=true"):
            self.assertIn(option, launch)
        self.assertEqual(deploy.read_digest(self.installer.database), self.original)
        self.assertEqual(deploy.read_digest(self.installer.stage / "preflight.db"), self.original)
        self.assertEqual(list(self.installer.checks.iterdir()), [])
        self.assertEqual(self.receipt()["status"], "preflight_passed")

    def test_preflight_schema_changes_never_reach_live_data(self):
        trial_database = None

        def run(command, **kwargs):
            nonlocal trial_database
            if command[0] == "systemd-run":
                value = next(argument for argument in command if argument.startswith("--setenv=MIRE_DATABASE="))
                trial_database = Path(value.split("=", 2)[2])
            elif command[:2] == ["systemctl", "stop"]:
                with closing(sqlite3.connect(trial_database)) as connection:
                    connection.execute("ALTER TABLE entries ADD COLUMN future_version TEXT")
            return ""

        with patch.object(self.installer, "run", side_effect=run):
            with self.assertRaisesRegex(deploy.DeploymentError, "review a migration separately"):
                self.installer.preflight(self.release, "a" * 40, self.installer.database)
        self.assertEqual(deploy.read_digest(self.installer.database), self.original)
        self.assertEqual(list(self.installer.checks.iterdir()), [])
        self.assertFalse(self.installer.pending.exists())


if __name__ == "__main__":
    unittest.main()
