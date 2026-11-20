#!/usr/bin/env python3
"""Fails if a workflow, the Action definition or a compose file is not valid YAML.

GitHub rejects an invalid action.yml or workflow only when it is first used, so
a typo would otherwise ship unnoticed.   python3 scripts/check-yaml.py
Needs PyYAML (pip install pyyaml).
"""
import glob
import sys

import yaml

files = sorted(
    glob.glob(".github/workflows/*.yml")
    + glob.glob("action.yml")
    + glob.glob("docker-compose*.yml")
    + glob.glob("docs/*.yml")
)
bad = 0
for f in files:
    try:
        doc = yaml.safe_load(open(f, encoding="utf-8"))
    except yaml.YAMLError as e:
        print(f"FAIL  {f}: {str(e).splitlines()[0]} ({getattr(e, 'problem_mark', '')})")
        bad += 1
        continue
    problem = None
    if f == "action.yml":
        for key in ("name", "description", "runs"):
            if key not in (doc or {}):
                problem = f"missing top-level '{key}'"
        steps = (doc or {}).get("runs", {}).get("steps", [])
        if (doc or {}).get("runs", {}).get("using") != "composite" or not steps:
            problem = "runs must be a composite action with steps"
        for i, st in enumerate(steps):
            if "run" in st and "shell" not in st:
                problem = f"step {i + 1} has run but no shell (composite steps need one)"
    elif f.startswith(".github/workflows/"):
        # PyYAML reads the bare key `on` as the boolean True.
        if not doc or ("on" not in doc and True not in doc) or "jobs" not in doc:
            problem = "a workflow needs 'on' and 'jobs'"
    if problem:
        print(f"FAIL  {f}: {problem}")
        bad += 1
    else:
        print(f"ok    {f}")
sys.exit(1 if bad else 0)
