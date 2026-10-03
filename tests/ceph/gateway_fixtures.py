"""Adapt only suite setup/cleanup to a single-user gateway.

Cleanup uses the configured main identity and removes versions/delete markers.
Test functions and assertions are untouched. Alternate identities remain
distinct; cross-user/public ACL scenarios are deliberately unsupported.
"""

import json
import os
from pathlib import Path


def pytest_collection_finish(session):
    report = os.environ.get("S3_GATEWAY_CEPH_REPORT")
    if report:
        Path(f"/reports/{report}-collection.json").write_text(json.dumps({"selected_tests": len(session.items)}))


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
