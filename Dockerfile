FROM docker.io/golang:1.22 AS build

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY ./main.go ./

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o ./dynamic-config-ping-exporter .

FROM docker.io/alpine:3.20.3 AS certificates
RUN apk add --no-cache ca-certificates

FROM scratch
COPY --from=certificates /etc/ssl /etc/ssl
COPY --from=build /app/dynamic-config-ping-exporter /dynamic-config-ping-exporter
ENTRYPOINT ["/dynamic-config-ping-exporter"]
