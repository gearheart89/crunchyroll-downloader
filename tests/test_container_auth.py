"""Container renewal adapter contracts; no real sign-in or account credentials."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

from test_cva_auth import ROOT, SESSION, RuntimeKind, bridge, credential_api


WATCH = "https://www.crunchyroll.com/watch/G12345678/fixture"
SOCKET_ENV = "CVA_DOWNLOADER_AUTH_SOCKET"


class ContainerRefreshTests(unittest.TestCase):
    def setUp(self):
        self.account = Mock()
        self.api = credential_api(self.account, RuntimeKind.CONTAINER)
        self.api.ContainerCredentialVault = Mock()
        self.api.KeyringCredentialStore = Mock()
        self.refresh = Mock(return_value={"status": "session_ok", "session": dict(SESSION)})
        self.module = SimpleNamespace(request_session_refresh=self.refresh)
        self.request = {"operation": "session-refresh", "source_url": WATCH}

    def run_bridge(self, request=None, socket_path="/run/cva/fixture/ipc"):
        environment = {} if socket_path is None else {SOCKET_ENV: socket_path}
        with patch.object(bridge, "load_cva", return_value=self.api), \
                patch.object(bridge, "load_browser_api") as windows, \
                patch.dict(sys.modules, {"crunchyroll_video_analyzer.container_auth": self.module}), \
                patch.dict(os.environ, environment, clear=True):
            result = bridge.run(ROOT, self.request if request is None else request)
        windows.assert_not_called()
        # Only the owning CVA process reads login credentials. The Go caller
        # validates the returned cookie before any existing session is replaced.
        self.account.get.assert_not_called()
        self.account.set.assert_not_called()
        self.account.delete.assert_not_called()
        self.api.ContainerCredentialVault.assert_not_called()
        self.api.KeyringCredentialStore.assert_not_called()
        return result

    def test_missing_and_rejected_session_forward_to_owned_connection(self):
        def refresh(source, rejected, *, environ):
            self.assertEqual(dict(environ), {SOCKET_ENV: "/run/cva/fixture/ipc"})
            return {"status": "session_ok", "session": dict(SESSION)}

        self.refresh.side_effect = refresh
        for force in (False, True):
            with self.subTest(force=force):
                self.refresh.reset_mock()
                self.assertEqual(self.run_bridge(dict(self.request, force_refresh=force)),
                                 {"status": "session_ok", "session": SESSION})
                self.assertEqual(self.refresh.call_args.args, (WATCH, force))
                self.assertEqual(self.refresh.call_count, 1)

    def test_missing_connection_and_other_runtimes_do_not_renew(self):
        self.assertEqual(self.run_bridge(socket_path=None), {"status": "session_refresh_unsupported"})
        self.assertEqual(self.run_bridge({"operation": "token", "source_url": WATCH}),
                         {"status": "browser_runtime_unsupported"})
        self.api.select_credential_store.return_value.runtime = RuntimeKind.POSIX_DESKTOP
        self.assertEqual(self.run_bridge(), {"status": "session_refresh_unsupported"})
        self.refresh.assert_not_called()

    def test_old_cva_without_container_api_reports_dependencies(self):
        self.module = None  # Import machinery raises ModuleNotFoundError.
        self.assertEqual(self.run_bridge(), {"status": "dependencies"})

    def test_invalid_request_never_constructs_store_or_calls_connection(self):
        for request, status in [
            ([], "protocol"), ({"operation": []}, "protocol"),
            ({"operation": "export"}, "protocol"),
            (dict(self.request, source_url=None), "source"),
            (dict(self.request, source_url=1), "source"),
            (dict(self.request, source_url=""), "source"),
            (dict(self.request, force_refresh="true"), "protocol"),
            (dict(self.request, force_refresh=1), "protocol"),
        ]:
            with self.subTest(status=status):
                self.assertEqual(self.run_bridge(request), {"status": status})
        self.api.default_credential_store.assert_not_called()
        self.refresh.assert_not_called()

    def test_failures_are_allowlisted_without_private_details(self):
        for status in sorted(bridge._CONTAINER_REFRESH_FAILURES):
            with self.subTest(status=status):
                self.refresh.return_value = {"status": status, "detail": "fixture-private-secret"}
                self.assertEqual(self.run_bridge(), {"status": status})
        self.refresh.side_effect = RuntimeError("fixture-private-secret")
        self.assertEqual(self.run_bridge(), {"status": "runtime"})

    def test_malformed_connection_response_is_never_forwarded(self):
        for value, status in [
            (None, "runtime"), ([], "runtime"), ({}, "runtime"),
            ({"status": "fixture-private-secret"}, "runtime"),
            ({"status": ["session_ok"]}, "runtime"),
            ({"status": "session_ok"}, "runtime"),
            ({"status": "session_ok", "session": SESSION, "detail": "fixture-private-secret"}, "runtime"),
            ({"status": "session_ok", "session": dict(SESSION, cookie="bad;cookie")}, "session_invalid"),
            ({"status": "session_ok", "session": dict(SESSION, expires_at=True)}, "session_invalid"),
        ]:
            with self.subTest(status=status):
                self.refresh.return_value = value
                self.assertEqual(self.run_bridge(), {"status": status})


@unittest.skipUnless(os.name == "posix" and os.environ.get("CVA_TEST_ROOT"),
                     "native Linux and CVA source required for private IPC integration")
class NativeContainerRefreshTests(unittest.TestCase):
    @unittest.skipUnless(os.environ.get("CRDL_TEST_BINARY"), "set CRDL_TEST_BINARY to check the compiled Linux helper")
    def test_binary_renews_expired_session_through_cva_without_exposing_secrets(self):
        cva_root = Path(os.environ["CVA_TEST_ROOT"])
        bridge.load_cva(cva_root)
        from crunchyroll_video_analyzer.container_auth import ContainerAuthenticationHandoff
        from crunchyroll_video_analyzer.credential_store import ContainerCredentialVault, CrunchyrollCredentials
        with tempfile.TemporaryDirectory(prefix="crdl-binary-auth-") as directory:
            root = Path(directory)
            root.chmod(0o700)
            key = root / "fixture.key"
            key.write_bytes(bytes(range(32)))
            key.chmod(0o400)
            vault = root / "credentials.vault"
            session_path = vault.with_name(vault.name + ".downloader-session")
            ContainerCredentialVault(session_path, key).set(CrunchyrollCredentials(
                bridge.SESSION_SCHEMA, json.dumps(dict(SESSION, expires_at=1)),
            ))
            previous_cache = session_path.read_bytes()
            # An expired fixture cookie skips the real token endpoint. The
            # broker supplies a controlled failure before any external I/O.
            refresh = Mock(return_value={"status": "verification_required"})
            environment = dict(os.environ, CVA_CONTAINER="true", CVA_CREDENTIAL_BACKEND="container-vault",
                               CVA_CREDENTIAL_VAULT_PATH=str(vault), CVA_CREDENTIAL_KEY_PATH=str(key))
            with ContainerAuthenticationHandoff(WATCH, refresh, directory=root) as path:
                environment[SOCKET_ENV] = str(path)
                completed = subprocess.run(
                    [os.environ["CRDL_TEST_BINARY"], "--auth-check", "--url", WATCH,
                     "--cva-root", str(cva_root), "--cva-python", sys.executable],
                    capture_output=True, text=True, timeout=30, env=environment, check=False,
                )
            self.assertEqual(refresh.call_count, 1)
            self.assertEqual(refresh.call_args.args, (WATCH, True))
            self.assertNotEqual(completed.returncode, 0)
            output = completed.stdout + completed.stderr
            self.assertIn("human verification", output)
            self.assertNotIn(SESSION["cookie"], output)
            self.assertNotIn(SESSION["device_id"], output)
            self.assertEqual(session_path.read_bytes(), previous_cache)

    def test_helper_uses_real_cva_connection_and_preserves_cache_ownership(self):
        cva_root = Path(os.environ["CVA_TEST_ROOT"])
        bridge.load_cva(cva_root)
        from crunchyroll_video_analyzer.container_auth import ContainerAuthenticationHandoff
        from crunchyroll_video_analyzer.credential_store import (
            ContainerCredentialVault, CrunchyrollCredentials,
        )
        with tempfile.TemporaryDirectory(prefix="crdl-auth-") as directory:
            root = Path(directory)
            root.chmod(0o700)
            key = root / "fixture.key"
            key.write_bytes(bytes(range(32)))
            key.chmod(0o400)
            vault = root / "credentials.vault"
            store = ContainerCredentialVault(vault, key)
            store.set(CrunchyrollCredentials("fixture-account", "fixture-password"))
            session_path = vault.with_name(vault.name + ".downloader-session")
            session_store = ContainerCredentialVault(session_path, key)
            session_store.set(CrunchyrollCredentials(bridge.SESSION_SCHEMA, json.dumps(SESSION)))
            original_account, original_session = vault.read_bytes(), session_path.read_bytes()
            refresh = Mock(return_value={"status": "session_ok", "session": dict(SESSION, cookie="fixture-renewed")})
            environment = dict(os.environ, CVA_CONTAINER="true", CVA_CREDENTIAL_BACKEND="container-vault",
                               CVA_CREDENTIAL_VAULT_PATH=str(vault), CVA_CREDENTIAL_KEY_PATH=str(key))
            environment.pop(SOCKET_ENV, None)

            def helper(operation, source=WATCH, force=True):
                completed = subprocess.run(
                    [sys.executable, "-I", "-B", str(ROOT / "cva_auth.py"), str(cva_root)],
                    input=json.dumps({"operation": operation, "source_url": source, "force_refresh": force}),
                    capture_output=True, text=True, timeout=20, env=environment, check=False,
                )
                self.assertEqual(completed.returncode, 0)
                self.assertEqual(completed.stderr, "")
                return json.loads(completed.stdout)

            with ContainerAuthenticationHandoff(WATCH, refresh, directory=root) as path:
                environment[SOCKET_ENV] = str(path)
                # Reading a valid cache never contacts the owning browser.
                self.assertEqual(helper("session-read"), {"status": "session_ok", "session": SESSION})
                refresh.assert_not_called()
                self.assertEqual(helper("session-refresh"), refresh.return_value)
                self.assertEqual(refresh.call_args.args, (WATCH, True))
                self.assertEqual(refresh.call_count, 1)
                # The broker binds exactly one watch request; a child cannot
                # reuse it to authenticate an unrelated source.
                self.assertEqual(helper("session-refresh", WATCH + "-other"), {"status": "protocol"})
                self.assertEqual(refresh.call_count, 1)
                refresh.return_value = {"status": "verification_required"}
                self.assertEqual(helper("session-refresh"), {"status": "verification_required"})
            self.assertEqual(helper("session-refresh"), {"status": "runtime"})
            self.assertEqual(vault.read_bytes(), original_account)
            self.assertEqual(session_path.read_bytes(), original_session)
            self.assertFalse(path.exists())


if __name__ == "__main__":
    unittest.main()
