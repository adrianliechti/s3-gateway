FROM golang:1.27.1-bookworm
WORKDIR /tests
COPY tests/iceberg/go.mod ./
COPY tests/iceberg/iceberg_test.go ./
RUN go mod tidy && go test -c -o /iceberg-tests .
COPY tests/iceberg/run.sh /run-tests.sh
ENTRYPOINT ["sh", "/run-tests.sh"]
