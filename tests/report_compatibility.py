"""Refresh the checked-in compatibility snapshot from Docker test artifacts."""

from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import re

ROOT = Path(__file__).resolve().parents[1]
REPORTS = ROOT / "results"


def artifact(path):
    return {
        "path": str(path.relative_to(ROOT)),
        "sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
    }


def ceph(backend, profile):
    path = REPORTS / f"ceph-{backend}-{profile}-summary.json"
    result = json.loads(path.read_text())
    required = ("selected_tests", "tests", "passed", "failures", "errors", "skipped", "exit_code")
    if any(key not in result for key in required):
        raise ValueError(f"Incomplete report: {path}; rerun the Ceph container")
    result.update(suite="ceph", backend=backend, artifact=artifact(path))
    if not result["selection_completed"]:
        result["status"] = "Stopped early"
    elif result["exit_code"] == 0:
        result["status"] = "Passed selection"
    else:
        result["status"] = "Failed selection"
    return result


def go_suite(suite, backend, version):
    path = REPORTS / f"{suite}-{backend}.json"
    events = [json.loads(line) for line in path.read_text().splitlines() if line.strip()]
    terminal = [e for e in events if e["Action"] in ("pass", "fail", "skip")]
    packages = [e for e in terminal if not e.get("Test")]
    if len(packages) != 1:
        raise ValueError(f"Missing or ambiguous package result: {path}")
    cases = [{"name": e["Test"], "outcome": e["Action"], "duration_seconds": e.get("Elapsed", 0)}
             for e in terminal if e.get("Test") and "/" not in e["Test"]]
    started = {e["Test"] for e in events if e["Action"] == "run" and "/" not in e.get("Test", "")}
    package = packages[0]
    return {
        "suite": suite, "backend": backend, "client_version": version,
        "started_utc": events[0]["Time"], "artifact": artifact(path),
        "selected_tests": len(started), "tests": len(cases),
        "passed": sum(c["outcome"] == "pass" for c in cases),
        "failures": sum(c["outcome"] == "fail" for c in cases),
        "errors": int(package["Action"] == "fail" and not any(c["outcome"] == "fail" for c in cases)),
        "skipped": sum(c["outcome"] == "skip" for c in cases),
        "selection_completed": len(started) == len(cases),
        "status": "Passed selection" if package["Action"] == "pass" else "Failed selection",
        "duration_seconds": package.get("Elapsed", 0), "cases": cases,
        "failure_output": [e["Output"] for e in events if e["Action"] == "output"
                           and any(c["outcome"] == "fail" and
                                   (e.get("Test", "") == c["name"] or e.get("Test", "").startswith(c["name"] + "/"))
                                   for c in cases)],
    }


def module_version(suite, module):
    contents = (ROOT / "tests" / suite / "go.mod").read_text()
    return re.search(r"\b" + re.escape(module) + r"\s+(v\S+)", contents).group(1)


def main():
    runs = [ceph(backend, profile) for profile in ("core", "versioning", "acl", "full") for backend in ("disk", "azure")]
    for suite, module in (("minio", "github.com/minio/minio-go/v7"), ("iceberg", "github.com/apache/iceberg-go")):
        for backend in ("disk", "azure"):
            if suite == "iceberg" and not (REPORTS / f"{suite}-{backend}.json").exists():
                continue
            runs.append(go_suite(suite, backend, module_version(suite, module)))
    for backend in ("disk", "azure"):
        runs.append(go_suite("protocol", backend, "gateway AWS SDK integration tests"))
    snapshot = {
        "generated_utc": datetime.now(timezone.utc).isoformat(),
        "scope": "Disk and Azurite Docker fixtures; Ceph with adapted cleanup and unchanged assertions; selected SDK scenarios. Not full S3 conformance or live Azure certification.",
        "runs": runs,
    }
    doc = ROOT / "COMPATIBILITY.md"
    contents = doc.read_text()
    start, end = "<!-- compatibility-results:start -->", "<!-- compatibility-results:end -->"
    if contents.count(start) != 1 or contents.count(end) != 1:
        raise ValueError("COMPATIBILITY.md must contain exactly one results block")
    lines = [f"Snapshot generated **{snapshot['generated_utc'][:10]}**. Dates of individual runs and failure details are retained in the [result snapshot](tests/compatibility-results.json).", "",
             "| Suite / profile | Backend | Selected | Executed | Passed | Failed | Errors | Skipped | Result |",
             "| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | --- |"]
    for run in runs:
        label = {"ceph": "Ceph " + run.get("profile", ""), "minio": "minio-go Docker", "iceberg": "Iceberg Docker", "protocol": "Protocol SDK"}[run["suite"]]
        backend = "Disk" if run["backend"] == "disk" else "Azure / Azurite"
        lines.append(f"| {label} | {backend} | {run['selected_tests']} | {run['tests']} | {run['passed']} | {run['failures']} | {run['errors']} | {run['skipped']} | {run['status']} |")
    full_runs = [run for run in runs if run["suite"] == "ceph" and run["profile"] == "full"]
    if all(run["selection_completed"] for run in full_runs):
        lines += ["", "**Both broader Ceph selections completed.** Every selected test has a recorded outcome; failures and skips are retained. This covers the configured S3 functional test file, not every suite in the upstream repository."]
    else:
        lines += ["", "**Broader Ceph results include incomplete selections.** Unexecuted tests have no result; counts from incomplete selections are not full-selection pass rates."]
    updated = contents.split(start)[0] + start + "\n" + "\n".join(lines) + "\n" + end + contents.split(end)[1]
    (ROOT / "tests" / "compatibility-results.json").write_text(json.dumps(snapshot, indent=2, ensure_ascii=False) + "\n")
    doc.write_text(updated)
    print("Updated COMPATIBILITY.md and tests/compatibility-results.json")


if __name__ == "__main__":
    main()
