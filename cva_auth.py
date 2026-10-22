"""Private, embedded authentication bridge. Never invoke to display a token.

CVA owns the credential and hosted-SSO implementation. This adapter imports it
from the user's installation; it does not copy secrets or modify that project.
"""

import contextlib
import json
import os
from pathlib import Path
import sys
from types import SimpleNamespace


class BridgeError(Exception):
    def __init__(self, status):
        self.status = status


def load_cva(root):
    sys.path.insert(0, str(root))
    # Select credentials before importing any Windows browser/runtime modules.
    # CVA is the single authority for overrides and container-marker detection.
    from crunchyroll_video_analyzer.credential_store import (
        CredentialRuntimeKind, CredentialStorageCorrupt,
        CredentialStorageLocked, CredentialStorageUnavailable,
        default_credential_store, select_credential_store,
    )
    return SimpleNamespace(**locals())


def load_browser_api():
    from crunchyroll_video_analyzer.browser_runtime import bind_launcher_runtime_to_cdp
    from crunchyroll_video_analyzer.configuration_source import SourceBoundConfigurationAcquirer
    from crunchyroll_video_analyzer.launcher import LauncherRuntimeSession
    from crunchyroll_video_analyzer.sso import (
        BrowserSsoProvider, DirectCdpAccountFlow, FirstPartyHostedSsoDriver,
        SsoInteractionRequired,
    )
    from crunchyroll_video_analyzer.windows_launcher import build_chrome_launch_contract, run_launcher_preflight
    from crunchyroll_video_analyzer.windows_runtime import create_windows_chrome_lifecycle
    from crunchyroll_video_analyzer.workbook_reconciliation import parse_crunchyroll_resource
    from crunchyroll_video_analyzer.workbook_reconciliation_plan import CreateSourceJobAction
    return SimpleNamespace(**locals())


def selected_credential_store(api):
    try:
        selection = api.select_credential_store()
    except ValueError:
        raise BridgeError("credential_config") from None
    try:
        store = api.default_credential_store(selection=selection)
    except ValueError:
        raise BridgeError("credential_config") from None
    except api.CredentialStorageUnavailable:
        raise BridgeError("credential_unavailable") from None
    except Exception:
        raise BridgeError("credential_store") from None
    return selection, store


def read_credentials(api, store):
    try:
        pair = store.get()
    except api.CredentialStorageLocked:
        raise BridgeError("credential_locked") from None
    except api.CredentialStorageCorrupt:
        raise BridgeError("credential_corrupt") from None
    except api.CredentialStorageUnavailable:
        raise BridgeError("credential_unavailable") from None
    except Exception:
        raise BridgeError("credential_store") from None
    if pair is None:
        raise BridgeError("credentials_missing")
    return pair


def acquire_token(api, source_url, application_root, credential_lookup):
    identity = api.parse_crunchyroll_resource(source_url)
    if identity is None:
        raise BridgeError("source")
    source = api.CreateSourceJobAction(1, source_url, identity)
    profile = application_root / "Browser Profile"

    def lifecycle_factory(endpoint):
        preflight = api.run_launcher_preflight()
        contract = api.build_chrome_launch_contract(
            preflight.chrome, endpoint, profile_path=profile,
            application_data_root=application_root,
        )
        lifecycle = api.create_windows_chrome_lifecycle(contract, application_root)
        lifecycle.mutex_name = r"Local\CrunchyrollDownloader.Launcher.Runtime"
        return lifecycle

    session = api.LauncherRuntimeSession(lifecycle_factory)
    try:
        runtime = session.start()
        browser = api.bind_launcher_runtime_to_cdp(runtime)
        try:
            with browser.connect_page(preferred_url_substring=None) as page:
                configuration = api.SourceBoundConfigurationAcquirer().acquire(page, source)
        except Exception:
            raise BridgeError("configuration") from None

        def connect_owned_page(requested_profile):
            if requested_profile.resolve() != profile.resolve():
                raise BridgeError("profile_error")
            return browser.connect_page(preferred_url_substring=None)

        provider = api.BrowserSsoProvider(
            api.DirectCdpAccountFlow(connect_owned_page, api.FirstPartyHostedSsoDriver(configuration)),
            profile_path=profile, application_data_root=application_root,
            credential_lookup=credential_lookup,
        )
        try:
            return provider.get_token()
        except api.SsoInteractionRequired as error:
            raise BridgeError(error.state.value) from None
    finally:
        # Includes failed startup cleanup. Windows Job ownership also bounds
        # Chrome's lifetime if the parent terminates this helper on timeout.
        session.close()


def run(root, request):
    try:
        api = load_cva(root)
    except Exception:
        return {"status": "dependencies"}
    try:
        operation = request.get("operation", "token")
        if operation not in {"token", "credentials-check"}:
            raise BridgeError("protocol")
        source_url = request.get("source_url")
        if operation == "token" and (not isinstance(source_url, str) or not source_url):
            raise BridgeError("source")
        selection, store = selected_credential_store(api)
        if operation == "credentials-check":
            # Confirm decryption/readability, but never serialize the pair.
            read_credentials(api, store)
            return {
                "status": "credentials_ok",
                "runtime": selection.runtime.value,
                "backend": selection.store.value,
            }
        if selection.runtime is not api.CredentialRuntimeKind.WINDOWS_DESKTOP:
            raise BridgeError("browser_runtime_unsupported")
        local_app_data = os.environ.get("LOCALAPPDATA")
        if not local_app_data:
            raise BridgeError("runtime")
        try:
            browser_api = load_browser_api()
        except Exception:
            raise BridgeError("dependencies") from None
        # Share CVA's selected credential store, never its profile/runtime state.
        application_root = Path(local_app_data) / "Crunchyroll Downloader"
        token = acquire_token(
            browser_api, source_url, application_root,
            lambda: read_credentials(api, store),
        )
        return {"status": "ok", "access_token": token}
    except BridgeError as error:
        return {"status": error.status}
    except BaseException:
        # Backend/browser exceptions may contain credentials, URLs or tokens.
        return {"status": "runtime"}


def main():
    # stdout is a protocol-only pipe, not a log. Suppress all library output and
    # serialize no arbitrary exceptions, account names, passwords or cookies.
    protocol = sys.stdout
    with open(os.devnull, "w", encoding="utf-8") as sink:
        with contextlib.redirect_stdout(sink), contextlib.redirect_stderr(sink):
            try:
                if protocol.isatty():
                    return 1
                request = json.load(sys.stdin)
                result = run(Path(sys.argv[1]).resolve(), request)
            except BaseException:
                result = {"status": "runtime"}
    protocol.write(json.dumps(result))
    protocol.flush()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
