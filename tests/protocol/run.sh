#!/bin/sh
mkdir -p /reports
report="/reports/${REPORT_NAME:-protocol}.json"
go tool test2json -t -p github.com/adrianliechti/s3-gateway/gateway /protocol-tests -test.v -test.run 'TestSDKVersion|TestSDKPrivateACL|TestSDKCompatibility' -test.timeout=5m > "$report"
result=$?
cat "$report"
exit "$result"
