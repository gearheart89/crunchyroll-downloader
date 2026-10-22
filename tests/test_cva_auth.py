"""Offline tests: never open Chrome, the network, or the real credential store."""

import contextlib
import importlib.util
import io
import os
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch


ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("bridge", ROOT / "cva_auth.py")
bridge = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bridge)


class Interaction(Exception):
    def __init__(self, state):
        self.state = SimpleNamespace(value=state)


class BridgeTests(unittest.TestCase):
    def setUp(self):
        self.store = Mock()
        self.store.get.return_value = SimpleNamespace(account="fixture-account", password="fixture-secret")
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
            KeyringCredentialStore=Mock(return_value=self.store),
            FirstPartyHostedSsoDriver=Mock(),
            DirectCdpAccountFlow=Mock(),
            BrowserSsoProvider=Mock(return_value=self.provider),
            SsoInteractionRequired=Interaction,
        )
        self.application_root = Path("fixture-app-data") / "Crunchyroll Downloader"

    def acquire(self):
        return bridge.acquire_token(self.api, "https://www.crunchyroll.com/series/FIXTURE", self.application_root)

    def test_session_reuse_is_lazy_and_isolated(self):
        self.assertEqual(self.acquire(), "fixture-token")
        self.api.KeyringCredentialStore.assert_not_called()
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
        self.api.KeyringCredentialStore.assert_not_called()
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


@unittest.skipUnless(os.environ.get("CVA_TEST_ROOT"), "set CVA_TEST_ROOT for local CVA API compatibility tests")
class LocalCVAContractTests(unittest.TestCase):
    def test_real_provider_reuses_session_then_reads_keyring_on_demand(self):
        # Load actual CVA classes, but inject a fake flow and fake credentials.
        api = bridge.load_cva(Path(os.environ["CVA_TEST_ROOT"]))
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
