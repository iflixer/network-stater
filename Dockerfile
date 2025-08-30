# build environment
FROM golang:1.25 AS build-env
WORKDIR /build
COPY src/go.mod ./
RUN go mod download
COPY src src
WORKDIR /build/src
RUN CGO_ENABLED=0 GOOS=linux go build -o /build/app .

FROM alpine:3.15
WORKDIR /app

COPY --from=build-env /build/app /app/network-stater

ENTRYPOINT [ "/app/network-stater" ]
