#!/usr/bin/env python3
"""Exchange an existing Docker Hub staging PAT for a short-lived login token."""

import argparse
import getpass
import http.client
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
import warnings

DEFAULT_LOGIN_URL = "https://hub-stage.docker.com/v2/users/login"
MAX_RESPONSE_BYTES = 1 << 20


class TokenError(Exception):
    pass


class NoRedirects(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def login_url(value):
    try:
        url = urllib.parse.urlsplit(value)
        valid = (
            url.scheme == "https"
            and re.fullmatch(r"(?:[a-z0-9-]+\.)*docker\.com", url.hostname or "")
            and url.port in (None, 443)
            and url.username is None
            and url.password is None
            and url.path in ("/v2/users/login", "/v2/users/login/")
            and not url.query
            and not url.fragment
            and not any(character.isspace() for character in value)
        )
    except ValueError:
        valid = False
    if not valid:
        raise argparse.ArgumentTypeError(
            "login URL must be HTTPS on docker.com or a subdomain, with "
            "path /v2/users/login and no credentials, query, or fragment"
        )
    return value


def prompt_credentials():
    try:
        with open("/dev/tty", "r", encoding="utf-8") as input_tty, open(
            "/dev/tty", "w", encoding="utf-8"
        ) as output_tty:
            output_tty.write("Docker Hub staging username: ")
            output_tty.flush()
            username = input_tty.readline().strip()
            # Never fall back to an echoed password prompt or piped stdin.
            with warnings.catch_warnings():
                warnings.simplefilter("error", getpass.GetPassWarning)
                secret = getpass.getpass("Existing staging PAT (hidden): ", stream=output_tty)
    except (OSError, EOFError, getpass.GetPassWarning):
        raise TokenError("a local terminal with hidden input is required") from None
    if not username:
        raise TokenError("username is required")
    if not secret.startswith("dckr_") or len(secret) <= len("dckr_"):
        raise TokenError("provide a Docker-issued staging PAT (dckr_...), not an account password")
    return username, secret


def exchange_token(endpoint, username, secret):
    payload = json.dumps({"username": username, "password": secret}).encode("utf-8")
    request = urllib.request.Request(
        endpoint,
        data=payload,
        headers={"Content-Type": "application/json", "User-Agent": "docker-agent-staging-token"},
        method="POST",
    )
    opener = urllib.request.build_opener(NoRedirects())
    try:
        with opener.open(request, timeout=30) as response:
            if response.status != 200:
                raise TokenError("login failed (HTTP {})".format(response.status))
            body = response.read(MAX_RESPONSE_BYTES + 1)
    except urllib.error.HTTPError as error:
        status = error.code
        error.close()
        if status in (401, 403):
            raise TokenError("staging credentials were rejected (HTTP {})".format(status)) from None
        if 300 <= status < 400:
            raise TokenError("login redirected; refusing to forward credentials") from None
        raise TokenError("login failed (HTTP {}); try again later".format(status)) from None
    except (OSError, urllib.error.URLError, http.client.HTTPException):
        raise TokenError("could not reach Hub over verified HTTPS; check connectivity and TLS") from None
    if len(body) > MAX_RESPONSE_BYTES:
        raise TokenError("login response was too large")
    try:
        parsed = json.loads(body)
    except (ValueError, UnicodeError):
        raise TokenError("login did not return valid JSON") from None
    token = parsed.get("token") if isinstance(parsed, dict) else None
    if not isinstance(token, str) or not token or any(character.isspace() for character in token):
        raise TokenError("login response did not contain a usable token")
    return token


def save_token(path, token):
    # Exclusive creation also refuses existing symlinks; never overwrite a secret.
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as output:
        output.write(token + "\n")


def main(argv=None):
    parser = argparse.ArgumentParser(
        description=__doc__,
        epilog=(
            "Requires Python 3 and a local terminal (macOS/Linux). This does not create a PAT: "
            "create one in the staging account's security settings first. "
            "Uses POST /v2/users/login with JSON username/password and reads the token field, "
            "as in pkg/hubauth. The default staging host comes from that package's tests; "
            "confirm it is the staging environment you use. Credentials stay in memory; "
            "TLS is verified and redirects are refused. No Docker credential store is read. "
            "Treat the resulting token as a secret; it expires and is not a registry login/PAT."
        ),
    )
    parser.add_argument(
        "--login-url", type=login_url, default=DEFAULT_LOGIN_URL,
        help="Docker HTTPS login endpoint (default: %(default)s)",
    )
    destination = parser.add_mutually_exclusive_group(required=True)
    destination.add_argument("--output", metavar="FILE", help="create a new private (0600) token file")
    destination.add_argument(
        "--stdout", action="store_true", help="emit only the token to redirected/captured stdout (not a terminal)",
    )
    args = parser.parse_args(argv)
    try:
        if args.stdout and sys.stdout.isatty():
            raise TokenError("redirect/capture stdout or use --output; refusing to display the token")
        username, secret = prompt_credentials()
        token = exchange_token(args.login_url, username, secret)
        if args.output:
            save_token(args.output, token)
            print("Saved short-lived Hub login token in a new private file.", file=sys.stderr)
        else:
            print(token)
    except TokenError as error:
        print("hub-staging-token: {}".format(error), file=sys.stderr)
        return 1
    except OSError:
        print("hub-staging-token: could not create/write output; use a new writable path", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print("\nhub-staging-token: cancelled", file=sys.stderr)
        return 130
    return 0


if __name__ == "__main__":
    sys.exit(main())
