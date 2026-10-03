#!/bin/sh
mkdir -p /reports
if [ -n "$GATEWAY_TEST_STORAGE_ENDPOINT" ]; then
  go tool test2json -t -p github.com/adrianliechti/s3-gateway/backend/aws /s3-backend-tests -test.v -test.timeout=5m > "/reports/${REPORT_NAME}-backend.json"
  backend_result=$?
  cat "/reports/${REPORT_NAME}-backend.json"
  if [ "$backend_result" -ne 0 ]; then exit "$backend_result"; fi
fi
report="/reports/${REPORT_NAME:-protocol}.json"
go tool test2json -t -p github.com/adrianliechti/s3-gateway/gateway /protocol-tests -test.v -test.run 'TestSDKVersion|TestSDKPrivateACL|TestSDKCompatibility|TestSDKBucket|TestSDKLifecycle' -test.timeout=10m > "$report"
result=$?
cat "$report"
exit "$result"
