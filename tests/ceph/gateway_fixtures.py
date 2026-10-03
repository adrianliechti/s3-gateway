"""Adapt only suite setup/cleanup to a single-user gateway.

Cleanup uses the configured main identity and removes versions/delete markers.
Test functions and assertions are untouched. Alternate identities remain
distinct; cross-user/public ACL scenarios are deliberately unsupported.
"""

import json
import os
from pathlib import Path

import pytest


# Explicit SigV2 users in the pinned upstream test file. Do not filter "v2":
# ListObjectsV2 and the ordinary SigV4 presigned/CORS tests remain in scope.
SIGV2_TESTS = {
    "test_cors_presigned_get_object_v2",
    "test_cors_presigned_get_object_tenant_v2",
    "test_cors_presigned_put_object_v2",
    "test_cors_presigned_put_object_tenant_v2",
    "test_bucket_logging_bucket_auth_type",  # Includes both SigV4 and SigV2.
}


@pytest.hookimpl(tryfirst=True)
def pytest_collection_modifyitems(config, items):
    config._gateway_excluded = []
    selection = os.environ.get("GATEWAY_RUSTFS_TESTS")
    if selection:
        names = {line.strip() for line in Path(selection).read_text().splitlines()
                 if line.strip() and not line.lstrip().startswith("#")}
        available = {item.originalname or item.name for item in items}
        config._gateway_unavailable = sorted(names - available)
        config._gateway_requested = len(names)
        selected, excluded = [], []
        for item in items:
            if (item.originalname or item.name) in names:
                selected.append(item)
            else:
                excluded.append(item)
                config._gateway_excluded.append({"name": item.nodeid, "reason": "outside pinned RustFS selection"})
        items[:] = selected
        if excluded:
            config.hook.pytest_deselected(items=excluded)
    if os.environ.get("GATEWAY_CEPH_INCLUDE_LEGACY") == "1":
        return
    selected, excluded = [], []
    for item in items:
        name = item.originalname or item.name
        reason = ("browser POST upload" if "post_object" in name else
                  "Signature V2" if name in SIGV2_TESTS else None)
        if reason:
            excluded.append(item)
            config._gateway_excluded.append({"name": item.nodeid, "reason": reason})
        else:
            selected.append(item)
    items[:] = selected
    if excluded:
        config.hook.pytest_deselected(items=excluded)


def pytest_deselected(items):
    if items:
        config = items[0].config
        config._gateway_deselected = getattr(config, "_gateway_deselected", 0) + len(items)


def pytest_collection_finish(session):
    report = os.environ.get("GATEWAY_CEPH_REPORT")
    if report:
        deselected = getattr(session.config, "_gateway_deselected", 0)
        Path(f"/reports/{report}-collection.json").write_text(json.dumps({
            "collected_tests": len(session.items) + deselected,
            "selected_tests": len(session.items),
            "deselected_tests": deselected,
            "excluded_cases": getattr(session.config, "_gateway_excluded", []),
            "requested_test_names": getattr(session.config, "_gateway_requested", None),
            "unavailable_tests": getattr(session.config, "_gateway_unavailable", []),
        }, indent=2) + "\n")


def pytest_configure(config):
    import s3tests.functional as suite

    def cleanup():
        client = suite.get_client()
        prefix = suite.get_prefix()
        for bucket in client.list_buckets()["Buckets"]:
            name = bucket["Name"]
            if not name.startswith(prefix):
                continue
            while True:
                page = client.list_object_versions(Bucket=name)
                objects = [{"Key": obj["Key"], "VersionId": obj["VersionId"]}
                           for obj in page.get("Versions", []) + page.get("DeleteMarkers", [])]
                if not objects:
                    break
                result = client.delete_objects(Bucket=name, Delete={"Objects": objects})
                if result.get("Errors"):
                    raise RuntimeError(f"cleanup failed: {result['Errors']}")
            client.delete_bucket(Bucket=name)

    suite.setup = cleanup
    suite.teardown = cleanup
