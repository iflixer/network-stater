# build environment
FROM golang:1.25 AS build-env
WORKDIR /build
COPY src/go.mod ./
RUN go mod download
COPY src src
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /network-stater .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /network-stater /network-stater
USER 65532:65532
ENTRYPOINT ["/network-stater"]
