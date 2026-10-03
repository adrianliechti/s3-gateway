#!/bin/sh
mkdir -p /reports
report="/reports/${REPORT_NAME:-minio}.json"
go tool test2json -t -p github.com/adrianliechti/s3-gateway/tests/minio /minio-tests -test.v -test.timeout=3m > "$report"
result=$?
cat "$report"
exit "$result"
