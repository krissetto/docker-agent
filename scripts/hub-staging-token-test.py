#!/usr/bin/env python3
"""Offline checks: python3 scripts/hub-staging-token-test.py"""

import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import stat
import sys
import tempfile
import unittest
import urllib.error
from unittest import mock

sys.dont_write_bytecode = True

spec = importlib.util.spec_from_file_location("hub_staging_token", Path(__file__).with_name("hub-staging-token.py"))
hub = importlib.util.module_from_spec(spec)
spec.loader.exec_module(hub)


class TokenTests(unittest.TestCase):
    def setUp(self):
        self.opener = mock.Mock()
        patcher = mock.patch.object(hub.urllib.request, "build_opener", return_value=self.opener)
        self.build_opener = patcher.start()
        self.addCleanup(patcher.stop)

    def response(self, body, status=200):
        response = io.BytesIO(body)
        response.status = status
        self.opener.open.return_value = response
        return response

    def test_exchange_protocol_and_no_redirect_handler(self):
        self.response(b'{"token":"fake.short.lived"}')
        username, secret = 'staging"user', 'dckr_pat_"test\\secret'
        self.assertEqual(hub.exchange_token(hub.DEFAULT_LOGIN_URL, username, secret), "fake.short.lived")
        request = self.opener.open.call_args.args[0]
        self.assertEqual(request.full_url, "https://hub-stage.docker.com/v2/users/login")
        self.assertEqual(request.method, "POST")
        self.assertEqual(request.get_header("Content-type"), "application/json")
        self.assertEqual(json.loads(request.data), {"username": username, "password": secret})
        self.assertEqual(self.opener.open.call_args.kwargs, {"timeout": 30})
        handler = self.build_opener.call_args.args[0]
        self.assertIsNone(handler.redirect_request(request, None, 307, "", {}, "https://other.docker.com"))

    def test_endpoint_validation(self):
        for url in (hub.DEFAULT_LOGIN_URL, "https://hub-stage.docker.com:443/v2/users/login/"):
            self.assertEqual(hub.login_url(url), url)
        for url in (
            "http://hub-stage.docker.com/v2/users/login",
            "https://docker.com.attacker.test/v2/users/login",
            "https://attacker.test/v2/users/login",
            "https://user:secret@hub-stage.docker.com/v2/users/login",
            "https://hub-stage.docker.com:8443/v2/users/login",
            "https://hub-stage.docker.com/v2/users/login?secret=x",
            "https://hub-stage.docker.com/v2/users/login#fragment",
            "https://hub-stage.docker.com/other",
            "https://hub-stage.docker.com:\n443/v2/users/login",
        ):
            with self.subTest(url=url), self.assertRaises(hub.argparse.ArgumentTypeError):
                hub.login_url(url)

    def test_bad_responses(self):
        for body in (b"not json", b"[]", b"{}", b'{"token":123}', b'{"token":""}', b'{"token":"secret\\nvalue"}', b"x" * (hub.MAX_RESPONSE_BYTES + 1)):
            with self.subTest(body=body[:50]):
                self.response(body)
                with self.assertRaises(hub.TokenError):
                    hub.exchange_token(hub.DEFAULT_LOGIN_URL, "user", "dckr_pat_fake")

    def test_http_errors_never_expose_response_body(self):
        for status in (301, 307, 401, 403, 429, 500):
            body = io.BytesIO(b"dckr_pat_do-not-log-this")
            self.opener.open.side_effect = urllib.error.HTTPError(hub.DEFAULT_LOGIN_URL, status, "secret", {}, body)
            with self.subTest(status=status), self.assertRaises(hub.TokenError) as caught:
                hub.exchange_token(hub.DEFAULT_LOGIN_URL, "user", "dckr_pat_fake")
            self.assertNotIn("do-not-log-this", str(caught.exception))
            self.assertTrue(body.closed)

    def test_transport_errors_are_sanitized(self):
        self.opener.open.side_effect = urllib.error.URLError("secret transport diagnostic")
        with self.assertRaises(hub.TokenError) as caught:
            hub.exchange_token(hub.DEFAULT_LOGIN_URL, "user", "dckr_pat_fake")
        self.assertNotIn("secret transport diagnostic", str(caught.exception))

    def test_prompt_rejects_password_without_network(self):
        with mock.patch("builtins.open", side_effect=[io.StringIO("user\n"), io.StringIO()]), mock.patch.object(hub.getpass, "getpass", return_value="account-password"):
            with self.assertRaisesRegex(hub.TokenError, "not an account password"):
                hub.prompt_credentials()
        self.build_opener.assert_not_called()

    def test_no_terminal_fails_without_network(self):
        with mock.patch("builtins.open", side_effect=OSError("no tty")):
            with self.assertRaisesRegex(hub.TokenError, "local terminal"):
                hub.prompt_credentials()
        self.build_opener.assert_not_called()

    def test_echoed_prompt_fallback_is_refused(self):
        def unsafe_prompt(*args, **kwargs):
            hub.warnings.warn("hidden input unavailable", hub.getpass.GetPassWarning)

        with mock.patch("builtins.open", side_effect=[io.StringIO("user\n"), io.StringIO()]), mock.patch.object(hub.getpass, "getpass", side_effect=unsafe_prompt):
            with self.assertRaisesRegex(hub.TokenError, "hidden input"):
                hub.prompt_credentials()
        self.build_opener.assert_not_called()

    def test_private_output_refuses_overwrite_and_symlink(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "token"
            hub.save_token(output, "fake.short.lived")
            self.assertEqual(output.read_text(), "fake.short.lived\n")
            self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)
            with self.assertRaises(FileExistsError):
                hub.save_token(output, "replacement")
            link = Path(directory) / "link"
            link.symlink_to(output)
            with self.assertRaises(FileExistsError):
                hub.save_token(link, "replacement")
            self.assertEqual(output.read_text(), "fake.short.lived\n")

    def test_main_stdout_and_output(self):
        with mock.patch.object(hub, "prompt_credentials", return_value=("user", "dckr_pat_fake")), tempfile.TemporaryDirectory() as directory:
            for args in (["--stdout"], ["--output", os.path.join(directory, "token")]):
                self.response(b'{"token":"fake.short.lived"}')
                stdout, stderr = io.StringIO(), io.StringIO()
                with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
                    self.assertEqual(hub.main(args), 0)
                self.assertNotIn("dckr_pat_fake", stdout.getvalue() + stderr.getvalue())
                if "--stdout" in args:
                    self.assertEqual(stdout.getvalue(), "fake.short.lived\n")
                else:
                    self.assertEqual(stdout.getvalue(), "")
                    self.assertEqual(Path(args[1]).read_text(), "fake.short.lived\n")
                    self.assertNotIn("fake.short.lived", stderr.getvalue())

    def test_terminal_stdout_is_refused_before_prompt(self):
        with mock.patch.object(hub, "prompt_credentials") as prompt, mock.patch.object(hub.sys.stdout, "isatty", return_value=True), contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(hub.main(["--stdout"]), 1)
        prompt.assert_not_called()
        self.build_opener.assert_not_called()

    def test_help_and_invalid_arguments_do_not_prompt(self):
        for args, code in ((["--help"], 0), (["--stdout", "--login-url", "https://attacker.test/login"], 2), ([], 2)):
            with self.subTest(args=args), mock.patch.object(hub, "prompt_credentials") as prompt, contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
                with self.assertRaises(SystemExit) as caught:
                    hub.main(args)
                self.assertEqual(caught.exception.code, code)
                prompt.assert_not_called()
        self.build_opener.assert_not_called()


if __name__ == "__main__":
    unittest.main()
