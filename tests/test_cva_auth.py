"""Offline tests: never open Chrome, the network, or the real credential store."""

import contextlib
from enum import Enum
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
from types import SimpleNamespace
import tempfile
import unittest
from unittest.mock import Mock, patch


ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("bridge", ROOT / "cva_auth.py")
bridge = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bridge)


class Interaction(Exception):
    def __init__(self, state):
        self.state = SimpleNamespace(value=state)


class Unavailable(Exception):
    pass


class Locked(Unavailable):
    pass


class Corrupt(Exception):
    pass


class RuntimeKind(Enum):
    WINDOWS_DESKTOP = "windows_desktop"
    CONTAINER = "container"
    POSIX_DESKTOP = "posix_desktop"


def credential_api(store, runtime=RuntimeKind.WINDOWS_DESKTOP):
    selection = SimpleNamespace(
        runtime=runtime,
        store=SimpleNamespace(value="container_vault" if runtime is RuntimeKind.CONTAINER else "os_keyring"),
    )
    return SimpleNamespace(
        select_credential_store=Mock(return_value=selection),
        default_credential_store=Mock(return_value=store),
        CredentialRuntimeKind=RuntimeKind,
        CredentialStorageUnavailable=Unavailable,
        CredentialStorageLocked=Locked,
        CredentialStorageCorrupt=Corrupt,
    )


class BridgeTests(unittest.TestCase):
    def setUp(self):
        self.store = Mock()
        self.store.get.return_value = SimpleNamespace(account="fixture-account", password="fixture-secret")
        self.credential_api = credential_api(self.store)
        self.browser = Mock()
        self.browser.connect_page.side_effect = lambda **kwargs: contextlib.nullcontext(Mock())
        self.session = Mock()
        self.provider = Mock()
        self.provider.get_token.return_value = "fixture-token"
        self.api = SimpleNamespace(
            parse_crunchyroll_resource=Mock(return_value=object()),
            CreateSourceJobAction=Mock(),
            run_launcher_preflight=Mock(return_value=SimpleNamespace(chrome=object())),
            build_chrome_launch_contract=Mock(),
            create_windows_chrome_lifecycle=Mock(),
            LauncherRuntimeSession=Mock(return_value=self.session),
            bind_launcher_runtime_to_cdp=Mock(return_value=self.browser),
            SourceBoundConfigurationAcquirer=Mock(return_value=Mock()),
            FirstPartyHostedSsoDriver=Mock(),
            DirectCdpAccountFlow=Mock(),
            BrowserSsoProvider=Mock(return_value=self.provider),
            SsoInteractionRequired=Interaction,
        )
        self.application_root = Path("fixture-app-data") / "Crunchyroll Downloader"

    def acquire(self):
        return bridge.acquire_token(
            self.api, "https://www.crunchyroll.com/series/FIXTURE", self.application_root,
            lambda: bridge.read_credentials(self.credential_api, self.store),
        )

    def test_session_reuse_is_lazy_and_isolated(self):
        self.assertEqual(self.acquire(), "fixture-token")
        self.store.get.assert_not_called()
        options = self.api.BrowserSsoProvider.call_args.kwargs
        self.assertEqual(options["profile_path"], self.application_root / "Browser Profile")
        self.assertEqual(options["application_data_root"], self.application_root)
        self.session.close.assert_called_once()
        factory = self.api.LauncherRuntimeSession.call_args.args[0]
        factory(object())
        self.assertEqual(self.api.create_windows_chrome_lifecycle.call_args.args[1], self.application_root)
        self.assertEqual(self.api.create_windows_chrome_lifecycle.return_value.mutex_name, r"Local\CrunchyrollDownloader.Launcher.Runtime")

    def test_credential_lookup_uses_existing_store_read_only(self):
        self.acquire()
        lookup = self.api.BrowserSsoProvider.call_args.kwargs["credential_lookup"]
        self.assertIs(lookup(), self.store.get.return_value)
        self.store.set.assert_not_called()
        self.store.delete.assert_not_called()

    def test_missing_and_failed_store_are_sanitized(self):
        self.acquire()
        lookup = self.api.BrowserSsoProvider.call_args.kwargs["credential_lookup"]
        for result, error, status in [(None, None, "credentials_missing"), (None, RuntimeError("fixture-secret"), "credential_store")]:
            self.store.get.return_value = result
            self.store.get.side_effect = error
            with self.assertRaises(bridge.BridgeError) as caught:
                lookup()
            self.assertEqual(caught.exception.status, status)
            self.assertNotIn("fixture-secret", str(caught.exception))

    def test_challenge_and_rejection_are_sanitized_and_closed(self):
        for state in ["bad_credentials", "verification_required", "interaction_required", "network_error"]:
            self.session.reset_mock()
            self.provider.get_token.side_effect = Interaction(state)
            with self.assertRaises(bridge.BridgeError) as caught:
                self.acquire()
            self.assertEqual(caught.exception.status, state)
            self.session.close.assert_called_once()

    def test_failed_configuration_and_startup_close_runtime(self):
        self.api.SourceBoundConfigurationAcquirer.return_value.acquire.side_effect = RuntimeError("fixture-secret")
        with self.assertRaises(bridge.BridgeError) as caught:
            self.acquire()
        self.assertEqual(caught.exception.status, "configuration")
        self.session.close.assert_called_once()
        self.store.get.assert_not_called()
        self.session.reset_mock()
        self.session.start.side_effect = RuntimeError("fixture-secret")
        with self.assertRaises(RuntimeError):
            self.acquire()
        self.session.close.assert_called_once()

    def test_invalid_source_never_starts_runtime(self):
        self.api.parse_crunchyroll_resource.return_value = None
        with self.assertRaises(bridge.BridgeError) as caught:
            self.acquire()
        self.assertEqual(caught.exception.status, "source")
        self.session.start.assert_not_called()

    def test_protocol_suppresses_library_output_and_exception_text(self):
        def noisy_failure(*args):
            print("fixture-secret")
            raise RuntimeError("fixture-secret")
        output = io.StringIO()
        with patch.object(bridge, "run", side_effect=noisy_failure), patch("sys.argv", ["bridge", str(ROOT)]), patch("sys.stdin", io.StringIO('{}')), patch("sys.stdout", output):
            self.assertEqual(bridge.main(), 0)
        self.assertEqual(output.getvalue(), '{"status": "runtime"}')

    def test_dependencies_fail_without_raw_exception(self):
        with patch.object(bridge, "load_cva", side_effect=ImportError("fixture-secret")):
            self.assertEqual(bridge.run(ROOT, {}), {"status": "dependencies"})


class CredentialSelectionTests(unittest.TestCase):
    def setUp(self):
        self.store = Mock()
        self.store.get.return_value = SimpleNamespace(account="fixture-account", password="fixture-secret")
        self.api = credential_api(self.store)

    def run_bridge(self, request):
        with patch.object(bridge, "load_cva", return_value=self.api):
            return bridge.run(ROOT, request)

    def test_each_platform_uses_cva_selection_and_factory_without_browser(self):
        for runtime in RuntimeKind:
            with self.subTest(runtime=runtime.value):
                self.store.reset_mock()
                self.api = credential_api(self.store, runtime)
                with patch.object(bridge, "load_browser_api", side_effect=AssertionError("browser must not load")):
                    result = self.run_bridge({"operation": "credentials-check"})
                self.assertEqual(result, {
                    "status": "credentials_ok", "runtime": runtime.value,
                    "backend": self.api.select_credential_store.return_value.store.value,
                })
                self.api.select_credential_store.assert_called_once_with()
                self.api.default_credential_store.assert_called_once_with(
                    selection=self.api.select_credential_store.return_value,
                )
                self.store.get.assert_called_once_with()
                self.store.set.assert_not_called()
                self.store.delete.assert_not_called()

    def test_bad_selection_and_bad_paths_fail_without_read(self):
        for method in ["select_credential_store", "default_credential_store"]:
            with self.subTest(method=method):
                self.api = credential_api(self.store)
                getattr(self.api, method).side_effect = ValueError("fixture-secret")
                self.assertEqual(self.run_bridge({"operation": "credentials-check"}), {"status": "credential_config"})
                self.store.get.assert_not_called()

    def test_missing_locked_corrupt_and_unavailable_stores_are_distinct(self):
        for error, status in [
            (None, "credentials_missing"),
            (Locked("fixture-secret"), "credential_locked"),
            (Corrupt("fixture-secret"), "credential_corrupt"),
            (Unavailable("fixture-secret"), "credential_unavailable"),
            (RuntimeError("fixture-secret"), "credential_store"),
        ]:
            with self.subTest(status=status):
                self.store.get.return_value = None
                self.store.get.side_effect = error
                self.assertEqual(self.run_bridge({"operation": "credentials-check"}), {"status": status})
                self.store.set.assert_not_called()
                self.store.delete.assert_not_called()

    def test_windows_sign_in_defers_selected_store_read_until_needed(self):
        with patch.dict(os.environ, {"LOCALAPPDATA": str(ROOT / ".tools" / "fake-appdata")}), \
                patch.object(bridge, "load_browser_api", return_value=object()), \
                patch.object(bridge, "acquire_token", return_value="fixture-token") as acquire:
            self.assertEqual(self.run_bridge({"source_url": "https://www.crunchyroll.com/series/FIXTURE"}), {"status": "ok", "access_token": "fixture-token"})
        self.store.get.assert_not_called()
        self.assertIs(acquire.call_args.args[3](), self.store.get.return_value)
        self.store.get.assert_called_once_with()

    def test_container_token_attempt_does_not_load_windows_runtime_or_read_secrets(self):
        self.api = credential_api(self.store, RuntimeKind.CONTAINER)
        with patch.object(bridge, "load_browser_api") as load_browser:
            result = self.run_bridge({"source_url": "https://www.crunchyroll.com/series/FIXTURE"})
        self.assertEqual(result, {"status": "browser_runtime_unsupported"})
        load_browser.assert_not_called()
        self.store.get.assert_not_called()

    def test_invalid_operation_never_constructs_store(self):
        self.assertEqual(self.run_bridge({"operation": "export-secret"}), {"status": "protocol"})
        self.api.default_credential_store.assert_not_called()


SESSION = {"cookie": "fixture-cookie-secret", "device_id": "fixture-device", "expires_at": 0}


class SessionStorageTests(unittest.TestCase):
    def setUp(self):
        self.account = Mock()
        self.account.vault_path = Path(tempfile.gettempdir()).resolve() / "credentials.vault"
        self.account.key_path = self.account.vault_path.with_name("fixture.key")
        self.session = Mock()
        self.session.get.return_value = None
        self.api = credential_api(self.account)
        self.api.KeyringCredentialStore = Mock(return_value=self.session)
        self.api.ContainerCredentialVault = Mock(return_value=self.session)
        self.api.CrunchyrollCredentials = lambda account, password: SimpleNamespace(account=account, password=password)

    def run_bridge(self, request):
        with patch.object(bridge, "load_cva", return_value=self.api):
            return bridge.run(ROOT, request)

    def test_keyring_session_has_its_own_namespace(self):
        result = self.run_bridge({"operation": "session-write", "session": SESSION})
        self.assertEqual(result, {"status": "session_saved"})
        self.api.KeyringCredentialStore.assert_called_once_with(service=bridge.SESSION_SERVICE)
        pair = self.session.set.call_args.args[0]
        self.assertEqual(pair.account, bridge.SESSION_SCHEMA)
        self.assertEqual(json.loads(pair.password), SESSION)
        self.account.get.assert_not_called()
        self.account.set.assert_not_called()
        self.account.delete.assert_not_called()

    def test_container_session_reuses_cva_vault_with_separate_path_and_same_key(self):
        self.api.select_credential_store.return_value = SimpleNamespace(runtime=RuntimeKind.CONTAINER, store=SimpleNamespace(value="container_vault"))
        self.assertEqual(self.run_bridge({"operation": "session-write", "session": SESSION}), {"status": "session_saved"})
        self.api.ContainerCredentialVault.assert_called_once_with(
            self.account.vault_path.with_name("credentials.vault.downloader-session"), self.account.key_path)
        self.api.KeyringCredentialStore.assert_not_called()
        self.account.set.assert_not_called()

    def test_session_read_needs_no_browser_or_account_password(self):
        self.session.get.return_value = SimpleNamespace(account=bridge.SESSION_SCHEMA, password=json.dumps(SESSION))
        with patch.object(bridge, "load_browser_api", side_effect=AssertionError("no browser")):
            self.assertEqual(self.run_bridge({"operation": "session-read"}), {"status": "session_ok", "session": SESSION})
        self.account.get.assert_not_called()
        self.session.set.assert_not_called()
        self.session.get.return_value = None
        self.assertEqual(self.run_bridge({"operation": "session-read"}), {"status": "session_missing"})

    def test_storage_errors_are_sanitized_and_never_delete_cache(self):
        for error, status in [(Locked("fixture-cookie-secret"), "credential_locked"), (Corrupt("fixture-cookie-secret"), "credential_corrupt"), (Unavailable("fixture-cookie-secret"), "credential_unavailable"), (RuntimeError("fixture-cookie-secret"), "credential_store")]:
            with self.subTest(status=status):
                self.session.get.side_effect = error
                self.assertEqual(self.run_bridge({"operation": "session-read"}), {"status": status})
                self.session.set.side_effect = error
                self.assertEqual(self.run_bridge({"operation": "session-write", "session": SESSION}), {"status": status})
                self.session.delete.assert_not_called()

    def test_malformed_cache_is_not_returned_or_silently_replaced(self):
        for pair in [SimpleNamespace(account="wrong", password=json.dumps(SESSION)), SimpleNamespace(account=bridge.SESSION_SCHEMA, password="fixture-cookie-secret")]:
            self.session.get.return_value = pair
            self.assertEqual(self.run_bridge({"operation": "session-read"}), {"status": "session_invalid"})
        self.session.set.assert_not_called()
        for changes in [{"cookie": "fixture\r\nsecret"}, {"cookie": "fixture;secret"}, {"device_id": ""}, {"expires_at": -1}, {"expires_at": float("inf")}, {"expires_at": True}]:
            self.assertEqual(self.run_bridge({"operation": "session-write", "session": dict(SESSION, **changes)}), {"status": "session_invalid"})
        self.session.set.assert_not_called()

    def test_refresh_does_not_save_until_go_validates_cookie(self):
        def acquire(*args, session_sink, force_refresh):
            self.assertTrue(force_refresh)
            session_sink(SESSION)
            return "fixture-bearer"
        with patch.dict(os.environ, {"LOCALAPPDATA": str(ROOT / ".tools" / "fake-appdata")}), \
                patch.object(bridge, "load_browser_api", return_value=object()), \
                patch.object(bridge, "acquire_token", side_effect=acquire):
            result = self.run_bridge({"operation": "session-refresh", "source_url": "https://www.crunchyroll.com/watch/FIXTURE", "force_refresh": True})
        self.assertEqual(result, {"status": "session_ok", "session": SESSION})
        self.session.set.assert_not_called()
        self.account.set.assert_not_called()

    def test_docker_refresh_without_handoff_is_unsupported_without_reading_password(self):
        self.api.select_credential_store.return_value = SimpleNamespace(runtime=RuntimeKind.CONTAINER, store=SimpleNamespace(value="container_vault"))
        with patch.dict(os.environ, {}, clear=True), patch.object(bridge, "load_browser_api") as browser:
            self.assertEqual(self.run_bridge({"operation": "session-refresh", "source_url": "https://www.crunchyroll.com/watch/FIXTURE"}), {"status": "session_refresh_unsupported"})
            browser.assert_not_called()
        self.account.get.assert_not_called()


class BrowserSessionTests(unittest.TestCase):
    def page(self, cookies):
        page = Mock()
        page.connection.send.return_value = {"cookies": cookies}
        return page

    def cookie(self, **overrides):
        return dict({"name": "etp_rt", "value": SESSION["cookie"], "domain": ".crunchyroll.com", "path": "/", "secure": True, "expires": -1}, **overrides)

    def test_capture_only_service_session_and_device(self):
        page = self.page([self.cookie(), self.cookie(name="device_id", value=SESSION["device_id"]), self.cookie(name="unrelated", value="ignore"), self.cookie(domain="crunchyroll.com.attacker.invalid", value="ignore")])
        self.assertEqual(bridge.read_browser_session(page), SESSION)
        page.connection.send.assert_called_once_with("Network.getCookies", {"urls": [bridge.COOKIE_URL]})

    def test_ambiguous_expired_insecure_or_malformed_cookie_fails(self):
        for cookies in [[], [self.cookie(), self.cookie()], [self.cookie(secure=False)], [self.cookie(expires=1)], [self.cookie(expires=float("nan"))], [self.cookie(value="bad;cookie")], [self.cookie(domain="attacker.invalid")]]:
            with self.subTest(cookies=len(cookies)), self.assertRaises(bridge.BridgeError):
                bridge.read_browser_session(self.page(cookies))

    def test_renewal_removes_only_owned_service_cookie(self):
        page = self.page([self.cookie(), self.cookie(name="device_id"), self.cookie(domain="attacker.invalid")])
        bridge.discard_rejected_browser_cookie(page)
        self.assertEqual(page.connection.send.call_count, 2)
        page.connection.send.assert_called_with("Network.deleteCookies", {"name": "etp_rt", "domain": ".crunchyroll.com", "path": "/"})

    def test_capture_happens_before_owned_runtime_is_closed(self):
        fixture = BridgeTests()
        fixture.setUp()
        page = self.page([self.cookie()])
        fixture.browser.connect_page.side_effect = lambda **kwargs: contextlib.nullcontext(page)
        captured = []
        def sink(session):
            fixture.session.close.assert_not_called()
            captured.append(session)
        self.assertEqual(bridge.acquire_token(fixture.api, "https://www.crunchyroll.com/watch/FIXTURE", fixture.application_root, lambda: None, session_sink=sink, force_refresh=True), "fixture-token")
        self.assertEqual(captured[0]["cookie"], SESSION["cookie"])
        fixture.session.close.assert_called_once()
        self.assertTrue(any(call.args[0] == "Network.deleteCookies" for call in page.connection.send.call_args_list))


@unittest.skipUnless(os.environ.get("CVA_TEST_ROOT"), "set CVA_TEST_ROOT for local CVA API compatibility tests")
class LocalCVAContractTests(unittest.TestCase):
    def test_actual_keyring_session_adapter_preserves_account_credentials(self):
        api = bridge.load_cva(Path(os.environ["CVA_TEST_ROOT"]))
        keyring_class = api.KeyringCredentialStore
        account_service = "Crunchyroll Video Analyzer/Crunchyroll"
        slots = {(account_service, "account"): "fixture-account", (account_service, "password"): "fixture-password"}
        original = dict(slots)
        backend = Mock()
        backend.get_password.side_effect = lambda service, name: slots.get((service, name))
        backend.set_password.side_effect = lambda service, name, value: slots.__setitem__((service, name), value)
        backend.delete_password.side_effect = lambda service, name: slots.pop((service, name), None)
        selection = api.select_credential_store(environment={"CVA_CREDENTIAL_BACKEND": "os-keyring"}, os_name="nt", marker_exists=lambda _: False)
        with patch.object(api, "select_credential_store", return_value=selection), \
                patch.object(api, "default_credential_store", return_value=object()), \
                patch.object(api, "KeyringCredentialStore", side_effect=lambda **kwargs: keyring_class(backend, **kwargs)), \
                patch.object(bridge, "load_cva", return_value=api):
            self.assertEqual(bridge.run(ROOT, {"operation": "session-write", "session": SESSION}), {"status": "session_saved"})
            self.assertEqual(bridge.run(ROOT, {"operation": "session-read"}), {"status": "session_ok", "session": SESSION})
        self.assertEqual({key: slots[key] for key in original}, original)

    def test_actual_detection_and_factory_forwarding(self):
        api = bridge.load_cva(Path(os.environ["CVA_TEST_ROOT"]))
        actual_selector = api.select_credential_store
        for environment, os_name, marker, runtime, store in [
            ({}, "nt", None, "windows_desktop", "os_keyring"),
            ({}, "posix", None, "posix_desktop", "os_keyring"),
            ({}, "posix", "/.dockerenv", "container", "container_vault"),
            ({}, "posix", "/run/.containerenv", "container", "container_vault"),
            ({"CVA_CONTAINER": "yes"}, "posix", None, "container", "container_vault"),
            ({"CVA_CREDENTIAL_BACKEND": "container-vault"}, "nt", None, "container", "container_vault"),
            ({"CVA_CREDENTIAL_BACKEND": "os-keyring"}, "posix", "/.dockerenv", "posix_desktop", "os_keyring"),
        ]:
            with self.subTest(environment=environment, marker=marker, os_name=os_name):
                selected = actual_selector(environment=environment, os_name=os_name, marker_exists=lambda path: path == Path(marker) if marker else False)
                self.assertEqual((selected.runtime.value, selected.store.value), (runtime, store))
                with patch.object(api, "select_credential_store", return_value=selected), \
                        patch.object(api, "default_credential_store", return_value=object()) as factory:
                    returned, _ = bridge.selected_credential_store(api)
                self.assertIs(returned, selected)
                factory.assert_called_once_with(selection=selected)

    @unittest.skipUnless(os.name == "posix", "native POSIX vault integration")
    def test_native_vault_created_by_cva_is_read_by_downloader(self):
        cva_root = Path(os.environ["CVA_TEST_ROOT"])
        api = bridge.load_cva(cva_root)
        from crunchyroll_video_analyzer.credential_store import CrunchyrollCredentials
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            root.chmod(0o700)
            key = root / "fixture.key"
            key.write_bytes(bytes(range(32)))
            key.chmod(0o400)
            vault = root / "credentials.vault"
            environment = {
                "CVA_CONTAINER": "true", "CVA_CREDENTIAL_BACKEND": "auto",
                "CVA_CREDENTIAL_VAULT_PATH": str(vault), "CVA_CREDENTIAL_KEY_PATH": str(key),
            }
            with patch.dict(os.environ, environment):
                writer = api.default_credential_store()
                writer.set(CrunchyrollCredentials("fixture-account", "fixture-secret"))
                original = vault.read_bytes()
                self.assertNotIn(b"fixture-secret", original)
                # The session uses CVA's actual AES-GCM implementation and key,
                # but its file and lock are distinct from the account/password.
                self.assertEqual(bridge.run(cva_root, {"operation": "session-write", "session": SESSION}), {"status": "session_saved"})
                self.assertEqual(bridge.run(cva_root, {"operation": "session-read"}), {"status": "session_ok", "session": SESSION})
                session_path = vault.with_name(vault.name + ".downloader-session")
                self.assertNotIn(SESSION["cookie"].encode(), session_path.read_bytes())
                self.assertEqual(session_path.stat().st_mode & 0o777, 0o600)
                self.assertEqual(vault.read_bytes(), original)
                self.assertEqual(writer.get().password, "fixture-secret")
                # run() constructs a fresh production store from the same paths.
                result = bridge.run(cva_root, {"operation": "credentials-check"})
                self.assertEqual(result, {"status": "credentials_ok", "runtime": "container", "backend": "container_vault"})
                self.assertEqual(vault.read_bytes(), original)
                binary = os.environ.get("CRDL_TEST_BINARY")
                if binary:
                    completed = subprocess.run(
                        [binary, "--credentials-check", "--cva-root", str(cva_root), "--cva-python", sys.executable],
                        capture_output=True, text=True, timeout=30, check=False,
                    )
                    self.assertEqual(completed.returncode, 0, completed.stdout + completed.stderr)
                    self.assertEqual(completed.stdout.strip(), "Stored CVA credentials are readable (container vault). No sign-in was attempted.")
                    self.assertEqual(completed.stderr, "")
                    self.assertEqual(vault.read_bytes(), original)
                key.chmod(0o600)
                key.write_bytes(bytes(reversed(range(32))))
                key.chmod(0o400)
                self.assertEqual(bridge.run(cva_root, {"operation": "credentials-check"}), {"status": "credential_locked"})
                key.chmod(0o600)
                key.write_bytes(bytes(range(32)))
                key.chmod(0o400)
                vault.write_bytes(b"invalid encrypted envelope")
                self.assertEqual(bridge.run(cva_root, {"operation": "credentials-check"}), {"status": "credential_corrupt"})
                vault.write_bytes(original)
                vault.chmod(0o644)
                self.assertEqual(bridge.run(cva_root, {"operation": "credentials-check"}), {"status": "credential_unavailable"})
                vault.chmod(0o600)
                writer.delete()
                self.assertEqual(bridge.run(cva_root, {"operation": "credentials-check"}), {"status": "credentials_missing"})

    @unittest.skipUnless(os.name == "nt", "Windows browser provider compatibility")
    def test_real_provider_reuses_session_then_reads_keyring_on_demand(self):
        # Load actual CVA classes, but inject a fake flow and fake credentials.
        bridge.load_cva(Path(os.environ["CVA_TEST_ROOT"]))
        api = bridge.load_browser_api()
        from crunchyroll_video_analyzer.sso import SignedInBearer, SsoState
        from crunchyroll_video_analyzer.live_access import AccessToken
        from crunchyroll_video_analyzer.credential_store import CrunchyrollCredentials, CVA_CRUNCHYROLL_SERVICE
        self.assertEqual(CVA_CRUNCHYROLL_SERVICE, "Crunchyroll Video Analyzer/Crunchyroll")
        root = ROOT / ".tools" / "fixture-profile-root"
        lookup = Mock(return_value=CrunchyrollCredentials("fixture-account", "fixture-password"))
        token = AccessToken("fixture-token", 3600, "Bearer", "US", "account")
        flow = Mock()
        flow.acquire.return_value = SignedInBearer(token)
        provider = api.BrowserSsoProvider(flow, profile_path=root / "Browser Profile", application_data_root=root, credential_lookup=lookup)
        self.assertEqual(provider.get_token(), "fixture-token")
        lookup.assert_not_called()
        flow.acquire.side_effect = [SignedInBearer(None, SsoState.INTERACTION_REQUIRED), SignedInBearer(token)]
        provider = api.BrowserSsoProvider(flow, profile_path=root / "Browser Profile", application_data_root=root, credential_lookup=lookup)
        self.assertEqual(provider.get_token(), "fixture-token")
        lookup.assert_called_once()


if __name__ == "__main__":
    unittest.main()
