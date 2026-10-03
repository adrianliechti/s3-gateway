#!/bin/sh
mkdir -p /reports
report="/reports/${REPORT_NAME:-iceberg}.json"
go tool test2json -t -p github.com/adrianliechti/s3-gateway/tests/iceberg /iceberg-tests -test.v -test.timeout=5m > "$report"
result=$?
cat "$report"
exit "$result"
