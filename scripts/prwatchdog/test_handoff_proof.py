"""Required proof must reject successful commands that did not execute its cases."""
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock
import handoff_proof as proof


class ExecutionProofTests(unittest.TestCase):
    def events(self):
        events = []
        for package, tests in proof.REQUIRED.items():
            for test in tests:
                events += [{"Package": package, "Test": test, "Action": "run"},
                           {"Package": package, "Test": test, "Action": "pass"}]
            events.append({"Package": package, "Action": "pass"})
        return events

    def test_complete(self):
        proof.validate(self.events())

    def test_empty_missing_failed_skipped_and_never_started(self):
        name = next(iter(proof.REQUIRED.values()))[0]
        for mode in ("empty", "missing", "fail", "skip", "never_started", "package_failed", "skipped_child"):
            with self.subTest(mode=mode):
                events = self.events()
                if mode == "empty":
                    events = []
                elif mode == "missing":
                    events = [e for e in events if e.get("Test") != name]
                elif mode == "never_started":
                    events = [e for e in events if not (e.get("Test") == name and e["Action"] == "run")]
                elif mode in ("fail", "skip"):
                    events += [{"Package": next(iter(proof.REQUIRED)), "Test": name, "Action": mode}]
                elif mode == "package_failed":
                    events += [{"Package": next(iter(proof.REQUIRED)), "Action": "fail"}]
                else:
                    events += [{"Package": next(iter(proof.REQUIRED)), "Test": name + "/boundary", "Action": "skip"}]
                with self.assertRaises(ValueError):
                    proof.validate(events)


    def test_runner_rejects_wrong_head_nonzero_empty_and_malformed_output(self):
        for mode in ("wrong_head", "nonzero", "empty", "malformed", "invalid_shape"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as directory:
                result = subprocess.CompletedProcess([], 1 if mode == "nonzero" else 0,
                    "invalid json" if mode == "malformed" else ("null\n" if mode == "invalid_shape" else ""), "")
                with mock.patch("sys.argv", ["proof", "--head", "expected", "--output", directory]), \
                     mock.patch.object(proof.subprocess, "check_output", return_value="other" if mode == "wrong_head" else "expected"), \
                     mock.patch.object(proof.subprocess, "run", return_value=result) as run:
                    self.assertEqual(proof.main(), 1)
                    report = json.loads((Path(directory) / "proof.json").read_text())
                    self.assertFalse(report["passed"])
                    self.assertEqual(report["head_sha"], "expected")
                    self.assertIn("error", report)
                    if mode == "wrong_head":
                        run.assert_not_called()


    def test_runner_records_exact_executed_set(self):
        events = self.events()
        responses = [subprocess.CompletedProcess([], 0,
            "\n".join(json.dumps(e) for e in events if e["Package"] == package), "")
            for package in proof.REQUIRED]
        with tempfile.TemporaryDirectory() as directory:
            with mock.patch("sys.argv", ["proof", "--head", "expected", "--output", directory]), \
                 mock.patch.object(proof.subprocess, "check_output", return_value="expected"), \
                 mock.patch.object(proof.subprocess, "run", side_effect=responses) as run:
                self.assertEqual(proof.main(), 0)
                report = json.loads((Path(directory) / "proof.json").read_text())
                self.assertTrue(report["passed"])
                self.assertEqual(report["required"], proof.REQUIRED)
                self.assertEqual(run.call_count, len(proof.REQUIRED))
                for command, (package, names) in zip(report["commands"], proof.REQUIRED.items()):
                    self.assertEqual(command["exit_code"], 0)
                    self.assertEqual(command["argv"][2], "./" + package.removeprefix(proof.PREFIX))
                    self.assertEqual(command["argv"][4], "^(" + "|".join(names) + ")$")
                    self.assertIn("-json", command["argv"])
                    self.assertIn("-count=1", command["argv"])


if __name__ == "__main__":
    unittest.main()
