FROM golang:1.27.1-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY backend ./backend
COPY gateway ./gateway
COPY internal ./internal
COPY cmd ./cmd
RUN CGO_ENABLED=0 go build -trimpath -o /gateway ./cmd/gateway

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/* && mkdir /data && chown 10001:10001 /data
COPY --from=build /gateway /usr/local/bin/gateway
USER 10001:10001
EXPOSE 9000
ENTRYPOINT ["gateway"]
CMD ["-listen", "0.0.0.0:9000", "-root", "/data"]
