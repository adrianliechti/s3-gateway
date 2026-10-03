FROM golang:1.27.1-bookworm
WORKDIR /source
COPY go.mod go.sum ./
RUN go mod download
COPY backend ./backend
COPY gateway ./gateway
RUN go test -c -o /protocol-tests ./gateway
COPY tests/protocol/run.sh /run-tests.sh
ENTRYPOINT ["sh", "/run-tests.sh"]
