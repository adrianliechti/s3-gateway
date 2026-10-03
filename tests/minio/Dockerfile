FROM golang:1.27.1-bookworm
WORKDIR /tests
COPY tests/minio/go.mod ./
COPY tests/minio/compatibility_test.go ./
RUN go mod tidy && go test -c -o /minio-tests .
COPY tests/minio/run.sh /run-tests.sh
ENTRYPOINT ["sh", "/run-tests.sh"]
