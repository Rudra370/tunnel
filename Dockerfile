FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN go mod tidy && CGO_ENABLED=0 go build -o /tunneld .

FROM alpine:3.20
COPY --from=builder /tunneld /usr/local/bin/tunneld
VOLUME /data
EXPOSE 3017 3018
ENTRYPOINT ["tunneld"]
CMD ["-host-key", "/data/host_key"]
