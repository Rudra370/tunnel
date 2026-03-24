FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /tunneld .

FROM alpine:3.20
COPY --from=builder /tunneld /usr/local/bin/tunneld
VOLUME /data
EXPOSE 2222 8080
ENTRYPOINT ["tunneld"]
CMD ["-host-key", "/data/host_key"]
