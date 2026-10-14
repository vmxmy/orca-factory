import itertools
import os
from pathlib import Path
import re
import subprocess
import unittest

WORKFLOW = Path(__file__).resolve().parents[2] / ".github/workflows/go.yml"


def run_step(name, **env):
    text = WORKFLOW.read_text()
    match = re.search(r"      - name: " + re.escape(name) + r"\n(?:(?!      - ).*\n)*?        run: \|\n((?:          .*\n|\n)+)", text)
    if not match:
        raise AssertionError(f"Missing runnable step: {name}")
    script = "\n".join(line[10:] for line in match[1].splitlines())
    return subprocess.run(["bash", "-euo", "pipefail", "-c", script],
                          env={**os.environ, **env}, text=True, capture_output=True)


class WorkflowTests(unittest.TestCase):
    def test_gate_results(self):
        for results in itertools.product(["success", "skipped", "failure", "cancelled"], repeat=4):
            with self.subTest(results=results):
                result = run_step("Check required results", NEEDS_RESULTS=" ".join(results))
                self.assertEqual(result.returncode == 0,
                                 not any(r in ("failure", "cancelled") for r in results), result.stderr)

    def test_ticket_warning(self):
        for body, warning in [("", True), ("Closes #n", True), ("No ticket", True),
                              ("Closes #123", False), ("Refs GH-123", False),
                              ("ZII-914", False), ("ABC-42", False),
                              ("$(exit 9)\n`exit 8`", True)]:
            with self.subTest(body=body):
                result = run_step("Check ticket reference", PR_BODY=body)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual("::warning::" in result.stdout, warning)


if __name__ == "__main__":
    unittest.main()
