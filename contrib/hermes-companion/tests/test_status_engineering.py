"""hermes vornik status names whether the memory key may start the review and
ingest workflows the hints ask for (design 24, "Engineering guidance from
Vornik", Decision; GitHub #686). An existing --no-delegate key is never
widened by the plugin: status says the hints are off and how to re-grant."""

import argparse
import unittest

from .helpers import FakeOpener, load_plugin

plugin = load_plugin()
from vornik_hermes import cli  # noqa: E402

DAEMON = {"companion-mcp": True}


def run(opener):
    out = []
    values = {"VORNIK_URL": "https://vornik.test", "VORNIK_BROKER_TOKEN": "", "VORNIK_MEMORY_TOKEN": "sk-m"}
    cli.status(argparse.Namespace(), values=values, opener=opener, out=out.append, load_config=lambda: {})
    return "\n".join(out)


class EngineeringStatusTest(unittest.TestCase):
    def test_granted_workflows_are_listed(self):
        op = FakeOpener(features=DAEMON, tools={"catalog": ({"workflows": [
            {"id": "companion-architectural-review"}, {"id": "companion-rag-ingest"}]}, False)})
        text = run(op)
        self.assertRegex(text, r"Review and ingest hints: on \(companion-architectural-review, companion-rag-ingest\)")

    def test_a_no_delegate_key_is_reported_with_the_re_grant_and_not_changed(self):
        op = FakeOpener(features=DAEMON, tools={"catalog": ("delegate_disabled: this key may not run tasks", True)})
        text = run(op)
        self.assertIn("Review and ingest hints: off", text)
        self.assertIn("vornikctl companion grant", text)
        self.assertIn("--workflows companion-architectural-review,companion-rag-ingest", text)
        self.assertEqual([c[0] for c in op.calls], ["catalog"], "status only reads")


if __name__ == "__main__":
    unittest.main()
