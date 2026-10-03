FROM golang:1.27.1-bookworm
WORKDIR /source
COPY go.mod go.sum ./
RUN go mod download
COPY backend ./backend
COPY gateway ./gateway
COPY internal ./internal
COPY notification ./notification
RUN go test -c -o /protocol-tests ./gateway
RUN go test -c -o /s3-backend-tests ./backend/aws
COPY tests/protocol/run.sh /run-tests.sh
ENTRYPOINT ["sh", "/run-tests.sh"]
