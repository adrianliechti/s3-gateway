#!/bin/sh
# Runs the conformance scenarios against CONFORMANCE_ENDPOINT (a gateway
# container) and writes the Go test events plus the protocol observation
# report to /reports. The report compares with CONFORMANCE_BASELINE when set.
mkdir -p /reports
name="${REPORT_NAME:-conformance}"
export CONFORMANCE_REPORT="/reports/${name}-observations.json"
go tool test2json -t -p github.com/adrianliechti/s3-gateway/tests/conformance /conformance-tests -test.v -test.timeout=15m > "/reports/${name}.json"
result=$?
cat "/reports/${name}.json"
exit "$result"
