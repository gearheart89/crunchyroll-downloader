"""Private, embedded authentication bridge. Never invoke to display a token.

CVA owns the credential and hosted-SSO implementation. This adapter imports it
from the user's installation; it does not copy secrets or modify that project.
"""

import contextlib
import json
import math
import os
from pathlib import Path
import sys
import time
from types import SimpleNamespace
import uuid


SESSION_SERVICE = "Crunchyroll Downloader/Crunchyroll Session"
SESSION_SCHEMA = "crunchyroll-downloader-session-v1"
COOKIE_URL = "https://www.crunchyroll.com/auth/v1/token"
_CONTAINER_REFRESH_FAILURES = frozenset({
    "session_refresh_unsupported", "session_unavailable", "session_invalid",
    "source", "protocol", "runtime", "configuration", "profile_error",
    "credential_config", "credential_unavailable", "credential_locked",
    "credential_corrupt", "credential_store", "credentials_missing",
    "bad_credentials", "verification_required", "interaction_required",
    "network_error", "site_error", "auth_flow_changed",
})


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
        CrunchyrollCredentials, KeyringCredentialStore, ContainerCredentialVault,
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


def selected_session_store(api, selection, account_store):
    """Reuse CVA encryption/permissions/locking, without changing its login pair."""
    try:
        if selection.store.value == "os_keyring":
            return api.KeyringCredentialStore(service=SESSION_SERVICE)
        return api.ContainerCredentialVault(
            account_store.vault_path.with_name(account_store.vault_path.name + ".downloader-session"),
            account_store.key_path,
        )
    except ValueError:
        raise BridgeError("credential_config") from None
    except Exception:
        raise BridgeError("credential_unavailable") from None


def valid_cookie(value):
    # RFC 6265 cookie-octet; reject instead of letting HTTP clients strip bytes.
    return (isinstance(value, str) and 0 < len(value) <= 16384
            and all(0x21 <= ord(char) <= 0x7e and char not in '\";,\\' for char in value))


def validate_session(value):
    if not isinstance(value, dict) or set(value) != {"cookie", "device_id", "expires_at"}:
        raise BridgeError("session_invalid")
    if not valid_cookie(value["cookie"]) or not valid_cookie(value["device_id"]):
        raise BridgeError("session_invalid")
    expiry = value["expires_at"]
    if type(expiry) is not int or not 0 <= expiry <= 253402300799:
        raise BridgeError("session_invalid")
    return value


def session_store_call(api, action):
    try:
        return action()
    except BridgeError:
        raise
    except api.CredentialStorageLocked:
        raise BridgeError("credential_locked") from None
    except api.CredentialStorageCorrupt:
        raise BridgeError("credential_corrupt") from None
    except api.CredentialStorageUnavailable:
        raise BridgeError("credential_unavailable") from None
    except Exception:
        raise BridgeError("credential_store") from None


def read_session(api, store):
    pair = session_store_call(api, store.get)
    if pair is None:
        return None
    if pair.account != SESSION_SCHEMA:
        raise BridgeError("session_invalid")
    try:
        value = json.loads(pair.password)
    except (ValueError, TypeError):
        raise BridgeError("session_invalid") from None
    return validate_session(value)


def write_session(api, store, value):
    value = validate_session(value)
    pair = api.CrunchyrollCredentials(SESSION_SCHEMA, json.dumps(value, separators=(",", ":")))
    session_store_call(api, lambda: store.set(pair))


def browser_session_cookies(page):
    result = page.connection.send("Network.getCookies", {"urls": [COOKIE_URL]})
    if not isinstance(result, dict) or not isinstance(result.get("cookies"), list):
        raise BridgeError("session_unavailable")
    # Only the dedicated downloader profile and the exact service's cookies.
    return [cookie for cookie in result["cookies"] if isinstance(cookie, dict)
            and cookie.get("domain") in {"crunchyroll.com", ".crunchyroll.com", "www.crunchyroll.com", ".www.crunchyroll.com"}
            and cookie.get("name") in {"etp_rt", "device_id"}]


def read_browser_session(page):
    cookies = browser_session_cookies(page)
    matches = [cookie for cookie in cookies if cookie["name"] == "etp_rt"]
    if len(matches) != 1 or matches[0].get("secure") is not True:
        raise BridgeError("session_unavailable")
    cookie = matches[0]
    expiry = cookie.get("expires", -1)
    if not isinstance(expiry, (int, float)) or not math.isfinite(expiry):
        raise BridgeError("session_invalid")
    expiry = max(0, int(expiry))
    if expiry and expiry <= time.time():
        raise BridgeError("session_unavailable")
    devices = {item.get("value") for item in cookies if item["name"] == "device_id" and valid_cookie(item.get("value"))}
    if len(devices) > 1:
        raise BridgeError("session_unavailable")
    return validate_session({"cookie": cookie.get("value"), "device_id": next(iter(devices), str(uuid.uuid4())), "expires_at": expiry})


def discard_rejected_browser_cookie(page):
    # A rejected saved cookie must not be recaptured from the downloader's
    # existing Chrome session. Other cookies and CVA's profile are untouched.
    for cookie in browser_session_cookies(page):
        if cookie["name"] == "etp_rt":
            page.connection.send("Network.deleteCookies", {
                "name": "etp_rt", "domain": cookie["domain"], "path": cookie.get("path", "/"),
            })


def refresh_container_session(source_url, force_refresh):
    # CVA's active invocation owns the broker and browser. The inherited path
    # carries no secret; session material stays in its private IPC and our pipe.
    if not os.environ.get("CVA_DOWNLOADER_AUTH_SOCKET"):
        raise BridgeError("session_refresh_unsupported")
    try:
        from crunchyroll_video_analyzer.container_auth import request_session_refresh
    except ImportError:
        raise BridgeError("dependencies") from None
    result = request_session_refresh(source_url, force_refresh, environ=os.environ)
    if not isinstance(result, dict):
        raise BridgeError("runtime")
    status = result.get("status")
    if status == "session_ok" and set(result) == {"status", "session"}:
        return {"status": "session_ok", "session": validate_session(result["session"])}
    if isinstance(status, str) and status in _CONTAINER_REFRESH_FAILURES:
        raise BridgeError(status)
    raise BridgeError("runtime")


def acquire_token(api, source_url, application_root, credential_lookup, *, session_sink=None, force_refresh=False):
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
                if force_refresh:
                    discard_rejected_browser_cookie(page)
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
            token = provider.get_token()
            if session_sink is not None:
                with browser.connect_page(preferred_url_substring="crunchyroll.com") as page:
                    session_sink(read_browser_session(page))
            return token
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
        if not isinstance(request, dict):
            raise BridgeError("protocol")
        operation = request.get("operation", "token")
        if not isinstance(operation, str) or operation not in {"token", "credentials-check", "session-read", "session-write", "session-refresh"}:
            raise BridgeError("protocol")
        source_url = request.get("source_url")
        if operation in {"token", "session-refresh"} and (not isinstance(source_url, str) or not source_url):
            raise BridgeError("source")
        force_refresh = request.get("force_refresh", False)
        if operation == "session-refresh" and type(force_refresh) is not bool:
            raise BridgeError("protocol")
        selection, store = selected_credential_store(api)
        if operation in {"session-read", "session-write"}:
            session_store = selected_session_store(api, selection, store)
            if operation == "session-read":
                value = read_session(api, session_store)
                return {"status": "session_missing"} if value is None else {"status": "session_ok", "session": value}
            write_session(api, session_store, request.get("session"))
            return {"status": "session_saved"}
        if operation == "credentials-check":
            # Confirm decryption/readability, but never serialize the pair.
            read_credentials(api, store)
            return {
                "status": "credentials_ok",
                "runtime": selection.runtime.value,
                "backend": selection.store.value,
            }
        if selection.runtime is not api.CredentialRuntimeKind.WINDOWS_DESKTOP:
            if operation == "session-refresh":
                if selection.runtime is api.CredentialRuntimeKind.CONTAINER:
                    # Go validates the result before writing the session cache.
                    return refresh_container_session(source_url, force_refresh)
                raise BridgeError("session_refresh_unsupported")
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
        if operation == "session-refresh":
            captured = []
            acquire_token(
                browser_api, source_url, application_root,
                lambda: read_credentials(api, store),
                session_sink=captured.append, force_refresh=force_refresh,
            )
            if len(captured) != 1:
                raise BridgeError("session_unavailable")
            # Go validates the new cookie with the token endpoint before saving
            # it. Failed sign-in or transport never replaces the previous cache.
            return {"status": "session_ok", "session": captured[0]}
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
    # serialize no arbitrary exceptions, account names or passwords. Session
    # operations carry a cookie only over this private parent/child pipe.
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
