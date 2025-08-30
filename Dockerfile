FROM golang:1.23-alpine AS build
WORKDIR /build
COPY go.mod ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /network-stater .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /network-stater /network-stater
USER 65532:65532
ENTRYPOINT ["/network-stater"]
