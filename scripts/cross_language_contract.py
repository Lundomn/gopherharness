#!/usr/bin/env python3
"""Compare Pico Python v3 and GopherHarness protocol behavior."""

import argparse
import json
import os
from pathlib import Path
import subprocess
import sys


def normalize_python(raw, parse):
    kind, value = parse(raw)
    if kind == "tool":
        return {"kind": "tools", "tools": [value]}
    if kind == "tools":
        return {"kind": "tools", "tools": value}
    return {"kind": kind, "text": value}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--python-root", default=os.environ.get("PICO_PYTHON_ROOT", "/Users/ljy/Documents/pico"))
    parser.add_argument("--go-command", default="go run ./cmd/gopherharness-contract")
    args = parser.parse_args()

    project = Path(__file__).resolve().parents[1]
    cases = json.loads((project / "contracts" / "model_output_cases.json").read_text(encoding="utf-8"))
    sys.path.insert(0, str(Path(args.python_root).resolve()))
    from pico.core.model_output import parse

    command = args.go_command.split()
    completed = subprocess.run(
        command,
        cwd=project,
        input=json.dumps(cases),
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if completed.returncode:
        raise SystemExit(f"Go contract runner failed:\n{completed.stderr}")
    go_rows = {row["name"]: {k: v for k, v in row.items() if k != "name"} for row in json.loads(completed.stdout)}

    failures = []
    for case in cases:
        expected = case["expected"]
        python_row = normalize_python(case["raw"], parse)
        go_row = go_rows[case["name"]]
        if python_row != expected or go_row != expected:
            failures.append({"name": case["name"], "expected": expected, "python": python_row, "go": go_row})
    if failures:
        print(json.dumps(failures, indent=2, ensure_ascii=False))
        return 1
    print(f"cross-language contract: {len(cases)} cases passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
