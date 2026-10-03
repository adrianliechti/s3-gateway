FROM golang:1.27.1-bookworm
WORKDIR /source
COPY go.mod go.sum ./
RUN go mod download
COPY backend ./backend
COPY gateway ./gateway
COPY internal ./internal
COPY notification ./notification
COPY tests/conformance ./tests/conformance
RUN go test -c -o /conformance-tests ./tests/conformance
COPY tests/conformance/run.sh /run-tests.sh
ENTRYPOINT ["sh", "/run-tests.sh"]
