#!/usr/bin/env python3
"""Run a prepared, checksummed dry-run pilot using the existing service ledger.

The bundle contains manifest.json, a static binary and isolated inspection JSON.
No package code runs here. Root only prepares private files and reads the service
environment; every application command runs as the existing service account.
"""
import argparse
import decimal
import hashlib
import json
import os
from pathlib import Path
import pwd
import stat
import subprocess
import tempfile


def read_regular(path, limit):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, "rb") as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_size > limit:
            raise RuntimeError(f"Not a bounded regular file: {path.name}")
        data = stream.read(limit + 1)
        if len(data) > limit:
            raise RuntimeError("File grew past its limit")
        return data


def write_atomic(path, data, uid, gid, mode):
    fd, name = tempfile.mkstemp(prefix=".pilot-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(data)
            stream.flush()
            os.fchown(stream.fileno(), uid, gid)
            os.fchmod(stream.fileno(), mode)
            os.fsync(stream.fileno())
        os.replace(name, path)
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def env_values(data):
    result = {}
    for line in data.decode().splitlines():
        key, sep, value = line.strip().removeprefix("export ").partition("=")
        if sep:
            result[key.strip()] = value.strip().strip("\"'")
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check-only", action="store_true")
    options = parser.parse_args()
    bundle = Path(__file__).resolve().parent
    manifest = json.loads(read_regular(bundle / "manifest.json", 65536))
    comparison_model = manifest.get("comparison_model")
    if comparison_model not in (None, "gpt-6.2-sol"):
        raise RuntimeError("Unsupported comparison model")
    blobs = {}
    for name, expected in manifest["files"].items():
        if Path(name).name != name or name in (".", ".."):
            raise RuntimeError("Invalid bundle filename")
        data = read_regular(bundle / name, 128 << 20 if name == "he-mod-index" else 2 << 20)
        if hashlib.sha256(data).hexdigest() != expected:
            raise RuntimeError(f"Checksum mismatch: {name}")
        blobs[name] = data
    for case in manifest["cases"]:
        inspection = json.loads(blobs[case["inspection"]])
        if inspection["sha256"] != case["artifact_sha256"]:
            raise RuntimeError("Artifact manifest mismatch")
        if case.get("context") and case["context"] not in blobs:
            raise RuntimeError("Missing fixture context")
        if comparison_model:
            baseline = json.loads(blobs[case["baseline"]])
            if baseline["sha256"] != inspection["sha256"] or baseline["model"] != "gpt-6.2-astra":
                raise RuntimeError("Comparison baseline mismatch")
    if options.check_only:
        print(json.dumps({"bundle_verified": True, "cases": len(manifest["cases"]), "model_calls": 0}))
        return
    if os.geteuid() != 0:
        raise RuntimeError("Run with sudo so the pilot can use the existing service ledger")
    service = pwd.getpwnam("he-mod-index")
    operator = pwd.getpwnam("nak")
    raw_env = read_regular(Path("/etc/he-mod-index.env"), 1 << 20)
    config = env_values(raw_env)
    if decimal.Decimal(config.get("INGEST_BUDGET_USD", "0")) != 20:
        raise RuntimeError("Expected the existing $20 cap; configuration was not changed")
    env = {"PATH": "/usr/bin:/bin", "DATA_DIR": "/srv/he-mod-index/data"}
    for key in ("OPENAI_KEY", "OPENAI_API_KEY"):
        if config.get(key):
            env[key] = config[key]
    if not any(key in env for key in ("OPENAI_KEY", "OPENAI_API_KEY")):
        raise RuntimeError("Service API key is missing")
    base = Path("/srv/he-mod-index/audit-pilot")
    base.mkdir(mode=0o750, exist_ok=True)
    os.chown(base, 0, service.pw_gid)
    work = base / ("sol-comparison-20261118" if comparison_model else "research-20261118")
    work.mkdir(mode=0o750, exist_ok=True)
    os.chown(work, 0, service.pw_gid)
    outputs = work / "results"
    outputs.mkdir(mode=0o700, exist_ok=True)
    os.chown(outputs, service.pw_uid, service.pw_gid)
    for name, data in blobs.items():
        write_atomic(work / name, data, 0, service.pw_gid, 0o550 if name == "he-mod-index" else 0o440)

    def command(args):
        return subprocess.run([str(work / "he-mod-index"), "audit", *args],
                              env=env, cwd=work, user=service.pw_uid,
                              group=service.pw_gid, extra_groups=[],
                              text=True, capture_output=True, timeout=540)

    status = command(["status"])
    if status.returncode:
        raise RuntimeError(status.stderr)
    before = json.loads(status.stdout)["spent_or_reserved_micros"]
    budget_path = work / "budget.json"
    if budget_path.exists():
        budget = json.loads(read_regular(budget_path, 4096))
    else:
        allowance = 1_000_000 if comparison_model else 6_000_000
        budget = {"starting_micros": before, "ceiling_micros": min(20_000_000, before + allowance)}
        write_atomic(budget_path, json.dumps(budget).encode(), 0, 0, 0o600)
    if not 0 < budget["ceiling_micros"] <= 20_000_000:
        raise RuntimeError("Invalid persistent pilot budget")
    cap = str(decimal.Decimal(budget["ceiling_micros"]) / 1_000_000)
    print(f"Dry-run audit pilot. Shared cumulative ceiling ${cap}; site remains online.", flush=True)
    export = bundle / "results"
    export.mkdir(mode=0o700, exist_ok=True)
    os.chown(export, operator.pw_uid, operator.pw_gid)
    outcomes = []
    usage_records = {}
    for index, case in enumerate(manifest["cases"]):
        filename = f"{index:02d}-report.json"
        output = outputs / filename
        output.unlink(missing_ok=True)
        args = ["compare" if comparison_model else "review", "--inspection", str(work / case["inspection"]),
                "--out", str(output), "--budget", cap]
        if comparison_model:
            args += ["--model", comparison_model, "--report", str(work / case["baseline"])]
        elif case.get("mod_id"):
            args += ["--mod", str(case["mod_id"]), "--url", case["url"]]
        else:
            args += ["--context", str(work / case["context"])]
        result = command(args)
        print(case["name"] + ": " + result.stdout.strip(), flush=True)
        if result.stderr:
            print(result.stderr.strip(), flush=True)
        report = None
        if output.exists():
            data = read_regular(output, 2 << 20)
            report = json.loads(data)
            # Never export an earlier run's output for a different artifact.
            if report["sha256"] != case["artifact_sha256"]:
                raise RuntimeError("Report identity mismatch")
            if report["model"] != (comparison_model or "gpt-6.2-astra"):
                raise RuntimeError("Report model mismatch")
            write_atomic(export / filename, data, operator.pw_uid, operator.pw_gid, 0o600)
            audit_cache = Path(env["DATA_DIR"]) / "audits"
            if comparison_model:
                audit_cache = audit_cache / "comparisons" / comparison_model
            diagnostic = audit_cache / case["artifact_sha256"] / "model-output.json"
            if diagnostic.exists() and report.get("error"):
                write_atomic(export / f"{index:02d}-model-output.json", read_regular(diagnostic, 2 << 20),
                             operator.pw_uid, operator.pw_gid, 0o600)
            if report.get("usage"):
                usage_records[report["usage"]["id"]] = report["usage"]
        checks = []
        if report and case.get("expected_state"):
            checks.append(report["state"] == case["expected_state"])
        if case.get("forbidden_state"):
            checks.append(bool(report) and report["state"] != case["forbidden_state"])
        if "expected_cached" in case:
            checks.append(bool(report) and report["cached"] == case["expected_cached"])
        model = (report or {}).get("result") or {}
        metadata = model.get("metadata") or {}
        if case.get("expected_name"):
            checks.append(metadata.get("name") == case["expected_name"])
        if case.get("forbidden_name"):
            checks.append(metadata.get("name") != case["forbidden_name"])
        categories = {f["category"] for f in model.get("findings", [])}
        checks += [c in categories for c in case.get("required_categories", [])]
        if case.get("require_security_finding"):
            checks.append(bool(categories - {"prompt_injection"}))
        outcomes.append({"case": case["name"], "command_ok": result.returncode == 0,
                         "state": (report or {}).get("state"),
                         "expectations_pass": all(checks) if checks else None})
    status = command(["status"])
    if status.returncode:
        raise RuntimeError(status.stderr)
    after = json.loads(status.stdout)["spent_or_reserved_micros"]
    summary = {"revision": manifest["revision"], "outcomes": outcomes,
               "shared_spent_or_reserved_usd": after / 1e6,
               "shared_change_since_start_usd": (after - budget["starting_micros"]) / 1e6,
               "pilot_cumulative_ceiling_usd": budget["ceiling_micros"] / 1e6,
               "application_cap_usd": 20,
               "configuration_preserved": read_regular(Path("/etc/he-mod-index.env"), 1 << 20) == raw_env,
               "listings_changed": False, "public_badges_changed": False}
    if comparison_model:
        summary.update({"model": comparison_model, "baseline_model": "gpt-6.2-astra",
                        "frozen_context": True, "unique_model_requests": len(usage_records),
                        "comparison_spent_or_reserved_usd": sum(u.get("actual_micros", u["reserved_micros"]) for u in usage_records.values()) / 1e6,
                        "comparison_input_tokens": sum(u["input_tokens"] for u in usage_records.values()),
                        "comparison_output_tokens": sum(u["output_tokens"] for u in usage_records.values())})
    write_atomic(export / "summary.json", json.dumps(summary, indent=2).encode(), operator.pw_uid, operator.pw_gid, 0o600)
    print(json.dumps(summary, indent=2), flush=True)


if __name__ == "__main__":
    main()
