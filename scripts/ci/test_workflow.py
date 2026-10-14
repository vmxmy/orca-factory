import itertools
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
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

    def test_push_reuses_identical_pr_head(self):
        squash, head = "a" * 40, "b" * 40
        def runs(*results):
            return {"check_runs": [{"id": i, "name": n, "conclusion": c} for i, (n, c) in enumerate(results)]}
        success = [("Driver build", "success"), ("Driver race", "success"), ("Driver vuln", "success")]
        passed = runs(*success)
        cases = {
            "identical tree, checks passed": ("t1", passed, True),
            "tree differs": ("t2", passed, False),
            "race failed": ("t1", runs(("Driver build", "success"), ("Driver race", "failure"),
                                       ("Driver vuln", "success")), False),
            "race rerun passed": ("t1", runs(("Driver race", "failure"), *success), True),
            "checks skipped": ("t1", runs(("Driver build", "skipped"), ("Driver race", "skipped"),
                                          ("Driver vuln", "skipped")), False),
            "check API fails": ("t1", None, False),
        }
        for name, (head_tree, check_runs, verified) in cases.items():
            with self.subTest(name), tempfile.TemporaryDirectory() as tmp:
                responses = {
                    f"repos/o/r/commits/{squash}/pulls": [{"merge_commit_sha": squash, "head": {"sha": head}}],
                    f"repos/o/r/git/commits/{squash}": {"tree": {"sha": "t1"}},
                    f"repos/o/r/git/commits/{head}": {"tree": {"sha": head_tree}},
                    f"repos/o/r/commits/{head}/check-runs?per_page=100": check_runs,
                }
                Path(tmp, "responses.json").write_text(json.dumps(responses))
                gh = Path(tmp, "gh")
                gh.write_text('#!/bin/bash\n[[ $1 == api && $3 == --jq ]] || exit 9\n'
                              f'jq -er --arg p "$2" \'.[$p] // error("HTTP 502")\' {tmp}/responses.json | jq -r "$4"\n')
                gh.chmod(0o755)
                output = Path(tmp, "output")
                result = run_step("Reuse verified PR head", REPO="o/r", SHA=squash, GITHUB_OUTPUT=str(output),
                                  PATH=f"{tmp}:{os.environ['PATH']}")
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual("verified=true" in output.read_text() if output.exists() else False,
                                 verified, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
