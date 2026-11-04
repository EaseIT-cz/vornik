"""Every request the plugin makes names the plugin in its User-Agent (GitHub #628).

2026-11-02: behind a Cloudflare zone, Python's default "Python-urllib/3.x"
signature is refused with "403 error code: 1010" before the request reaches
the daemon. The Claude Code bundle's /upload failed that way; this client sent
the same signature. Found at: production (the Claude Code bundle). Here the
single request path, VornikClient._request, is the only HTTP call site.
"""
import os
import re
import unittest

from .helpers import FakeOpener, load_plugin

load_plugin()
from vornik_hermes.vornik_client import VornikClient  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))


def manifest_version():
    with open(os.path.join(HERE, "..", "plugin.yaml"), encoding="utf-8") as fh:
        return re.search(r"^version:\s*(\S+)", fh.read(), re.M).group(1)


class UserAgentTest(unittest.TestCase):
    def test_every_request_names_the_plugin_and_its_manifest_version(self):
        want = "vornik-companion/%s (hermes)" % manifest_version()
        seen = []
        opener = FakeOpener(features={})

        def recording(req, timeout=None):
            seen.append(req.get_header("User-agent"))
            return opener(req, timeout=timeout)

        client = VornikClient("http://vornik.test", "sk-vornik-test", opener=recording)
        client.capabilities()
        client.call("recall", {"query": "x"})
        self.assertGreaterEqual(len(seen), 2, "the client made no request to examine")
        self.assertEqual(set(seen), {want})

    def test_no_module_reaches_the_network_except_vornik_client(self):
        # One request path is what makes the test above cover the bundle. A
        # second client elsewhere would send its own default signature
        # (review-20261103-3fdd).
        bundle = os.path.join(HERE, "..")
        pattern = re.compile(
            r"urlopen|urllib\.request|urllib3|requests\.(get|post)|httpx|aiohttp|http\.client|socket\.create_connection")
        examined, offenders = 0, []
        for dirpath, dirnames, filenames in os.walk(bundle):
            dirnames[:] = [d for d in dirnames if d not in ("tests", "__pycache__")]
            for name in sorted(filenames):
                if not name.endswith(".py"):
                    continue
                examined += 1
                path = os.path.join(dirpath, name)
                rel = os.path.relpath(path, bundle)
                with open(path, encoding="utf-8") as fh:
                    if pattern.search(fh.read()) and rel != "vornik_client.py":
                        offenders.append(rel)
        self.assertGreater(examined, 1, "examined no module; the scan looks in the wrong place")
        self.assertEqual(offenders, [], "these modules reach the network outside VornikClient")


if __name__ == "__main__":
    unittest.main()
