"""Run upstream assertions with a documented single-user, version-aware cleanup adapter."""
import argparse
import configparser
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import xml.etree.ElementTree as ET
from urllib.parse import urlsplit

import boto3
from botocore.config import Config

endpoint = os.environ["S3_ENDPOINT"]
access = os.environ["S3_ACCESS_KEY"]
secret = os.environ["S3_SECRET_KEY"]
url = urlsplit(endpoint)
config = configparser.RawConfigParser()
config.read("/suite/s3tests.conf.SAMPLE")
config["DEFAULT"].update({"host": url.hostname, "port": str(url.port or (443 if url.scheme == "https" else 80)), "is_secure": str(url.scheme == "https"), "ssl_verify": "True"})
config["fixtures"]["bucket prefix"] = "s3gw-{random}-"
config["s3 main"].update({"access_key": access, "secret_key": secret, "display_name": access, "user_id": hashlib.sha256(access.encode()).hexdigest(), "email": "gateway@example.invalid", "api_name": "us-east-1"})
# Keep the upstream alternate identities distinct. Multi-user ACL/IAM tests
# should fail honestly until the gateway implements those capabilities.
filename = "/tmp/s3tests.conf"
with open(filename, "w") as output:
    config.write(output)
os.chmod(filename, 0o600)
os.environ["S3TEST_CONF"] = filename
client = boto3.client("s3", endpoint_url=endpoint, aws_access_key_id=access, aws_secret_access_key=secret, region_name="us-east-1", config=Config(signature_version="s3v4", connect_timeout=2, read_timeout=2, retries={"max_attempts": 0}))
for attempt in range(60):
    try:
        client.list_buckets()
        break
    except Exception:
        if attempt == 59:
            raise
        time.sleep(1)

parser = argparse.ArgumentParser()
profiles = parser.add_mutually_exclusive_group()
profiles.add_argument("--core", action="store_true")
profiles.add_argument("--full", action="store_true")
profiles.add_argument("--versioning", action="store_true")
profiles.add_argument("--acl", action="store_true")
options, extra = parser.parse_known_args()
profile = "core" if options.core else "versioning" if options.versioning else "acl" if options.acl else "full"
if options.core:
    names = ["test_bucket_list_empty", "test_bucket_list_distinct", "test_bucket_list_many", "test_bucket_listv2_many", "test_basic_key_count", "test_bucket_list_delimiter_prefix", "test_bucket_listv2_delimiter_prefix", "test_bucket_list_encoding_basic", "test_bucket_listv2_encoding_basic"]
    args = ["s3tests/functional/test_s3.py::" + name for name in names]
elif options.versioning:
    names = ["test_versioning_bucket_create_suspend", "test_versioning_obj_create_read_remove", "test_versioning_obj_create_read_remove_head", "test_versioning_stack_delete_merkers", "test_versioning_obj_plain_null_version_removal", "test_versioning_obj_plain_null_version_overwrite", "test_versioning_obj_plain_null_version_overwrite_suspended", "test_versioning_obj_suspend_versions", "test_versioning_obj_suspended_copy", "test_versioning_obj_create_versions_remove_all", "test_versioning_obj_create_versions_remove_special_names", "test_versioning_obj_create_overwrite_multipart", "test_versioning_obj_list_marker", "test_versioning_copy_obj_version", "test_versioning_multi_object_delete", "test_versioning_multi_object_delete_with_marker", "test_versioning_multi_object_delete_with_marker_create", "test_versioning_bucket_atomic_upload_return_version_id", "test_versioning_bucket_multipart_upload_return_version_id", "test_object_copy_versioned_bucket", "test_object_copy_versioned_url_encoding", "test_object_copy_versioning_multipart_upload", "test_multipart_copy_versioned", "test_delete_marker_versioned", "test_delete_object_version_if_match", "test_delete_objects_version_if_match", "test_bucket_list_return_data_versioning", "test_versioning_concurrent_multi_object_delete"]
    args = ["s3tests/functional/test_s3.py::" + name for name in names]
elif options.acl:
    names = ["test_bucket_acl_default", "test_object_acl_default", "test_bucket_acl_canned_private_to_private", "test_object_put_acl_mtime"]
    args = ["s3tests/functional/test_s3.py::" + name for name in names]
else:
    args = ["s3tests/functional/test_s3.py"]
report = os.environ.get("REPORT_NAME", "ceph") + "-" + profile
Path("/reports").mkdir(exist_ok=True)
Path(f"/reports/{report}-revision.txt").write_text(Path("/suite-revision").read_text())
with open(f"/reports/{report}-packages.txt", "w") as output:
    subprocess.run([sys.executable, "-m", "pip", "freeze"], stdout=output, check=True)
os.environ["PYTHONPATH"] = "/:" + os.environ.get("PYTHONPATH", "")
os.environ["S3_GATEWAY_CEPH_REPORT"] = report
fixture = "gateway_fixtures: main identity cleanup via ListObjectVersions/DeleteObjects; upstream assertions unchanged; alternate identities remain distinct"
Path(f"/reports/{report}-fixture.txt").write_text(fixture + ".\n")
command = [sys.executable, "-m", "pytest", "-p", "gateway_fixtures", "-v", "--tb=short", f"--junitxml=/reports/{report}.xml", *args, *extra]
started = datetime.now(timezone.utc).isoformat()
result = subprocess.run(command)
summary = {"started_utc": started, "endpoint": endpoint, "profile": profile, "suite_revision": Path("/suite-revision").read_text().strip(), "fixture": fixture, "pytest_arguments": command[3:], "exit_code": result.returncode}
collection = Path(f"/reports/{report}-collection.json")
if collection.exists():
    summary.update(json.loads(collection.read_text()))
xml = Path(f"/reports/{report}.xml")
if xml.exists():
    root = ET.parse(xml).getroot()
    suites = [root] if root.tag == "testsuite" else root.findall("testsuite")
    for key in ("tests", "failures", "errors", "skipped"):
        summary[key] = sum(int(s.get(key, 0)) for s in suites)
    summary["passed"] = summary["tests"] - summary["failures"] - summary["errors"] - summary["skipped"]
    summary["selection_completed"] = summary["tests"] == summary.get("selected_tests")
    summary["duration_seconds"] = sum(float(s.get("time", 0)) for s in suites)
    summary["unsuccessful_cases"] = [{"name": c.get("classname", "") + "::" + c.get("name", ""), "outcome": child.tag, "message": child.get("message", "")} for c in root.iter("testcase") for child in c if child.tag in ("failure", "error", "skipped")]
Path(f"/reports/{report}-summary.json").write_text(json.dumps(summary, indent=2) + "\n")
sys.exit(result.returncode)
