#!/usr/bin/env python3
"""Execute and validate the fork's fixed, focused handoff/config evidence set."""
import argparse
import json
from pathlib import Path
import subprocess
import sys

PREFIX = "github.com/gastownhall/gascity/"
REQUIRED = {
    PREFIX + "cmd/gc": [
        "TestCmdHandoffAutoSendsMailWithoutBlocking",
        "TestCmdHandoffAutoHookFormatCodex",
        "TestDoHandoffAutoReportsHookOutputWriteError",
        "TestDoHandoffAutoRecordsMailCreationFailure",
        "TestDoHandoff_Regression744_NamedSessionSkipsRestart",
        "TestDoHandoffWithRecycle_NamedSessionRequestsRestart",
        "TestHandoffRecycleRejectsNonSelfModes",
        "TestDoHandoff_NamedSessionClearRestartFailureReturnsError",
        "TestDoHandoff_NamedAlwaysSessionRequestsRestart",
    ],
    PREFIX + "internal/config": [
        "TestResolveContextAdvisoryPerAgentOverridesGlobal",
        "TestDefaultContextAdvisoryPreservesThresholdBoundaries",
        "TestParseContextAdvisoryAtGlobalAndAgentScope",
        "TestMergeAgentDefaultsContextAdvisoryPreservesUnsetFields",
        "TestParseRejectsInvalidContextAdvisory",
    ],
}


def validate(events):
    """Require execution and success of each case and package, without skips."""
    started, passed, packages = set(), set(), set()
    for event in events:
        if not isinstance(event, dict):
            raise ValueError("Go JSON event must be an object")
        package, test, action = event.get("Package"), event.get("Test"), event.get("Action")
        if action in ("fail", "skip"):
            raise ValueError(f"{package}/{test or '(package)'}: {action}")
        if test and action == "run":
            started.add((package, test))
        if test and action == "pass":
            passed.add((package, test))
        if not test and action == "pass":
            packages.add(package)
    for package, tests in REQUIRED.items():
        if package not in packages:
            raise ValueError(f"package did not pass: {package}")
        for test in tests:
            if (package, test) not in started or (package, test) not in passed:
                raise ValueError(f"required test did not execute and pass: {package}/{test}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--head", required=True)
    parser.add_argument("--output", default="handoff-config-proof")
    args = parser.parse_args()
    output = Path(args.output)
    output.mkdir(parents=True, exist_ok=True)
    report = {"head_sha": args.head, "required": REQUIRED, "passed": False, "commands": []}
    try:
        actual = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
        if actual != args.head:
            raise ValueError(f"checkout head {actual} differs from required head {args.head}")
        events = []
        for package, tests in REQUIRED.items():
            command = ["go", "test", "./" + package.removeprefix(PREFIX), "-run", "^(" + "|".join(tests) + ")$", "-count=1", "-json"]
            result = subprocess.run(command, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False)
            name = package.removeprefix(PREFIX).replace("/", "-")
            (output / (name + ".jsonl")).write_text(result.stdout)
            (output / (name + ".stderr")).write_text(result.stderr)
            report["commands"].append({"argv": command, "exit_code": result.returncode})
            print(result.stderr, file=sys.stderr, end="")
            if result.returncode != 0:
                raise ValueError(f"{package}: go test exited {result.returncode}; see {output}")
            events.extend(json.loads(line) for line in result.stdout.splitlines())
        validate(events)
        report["passed"] = True
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        report["error"] = str(error)
        print(f"handoff/config proof: {error}", file=sys.stderr)
    (output / "proof.json").write_text(json.dumps(report, indent=2) + "\n")
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    sys.exit(main())
