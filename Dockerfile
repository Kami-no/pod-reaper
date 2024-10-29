# build stage
FROM golang:1.23-bookworm AS build

WORKDIR /opt/build

COPY ["go.mod", "go.sum", "./"]
RUN go mod download

COPY . .
RUN go test ./... \
    && CGO_ENABLED=0 go build -a -tags 'netgo' -ldflags '-s -w' -o app

# artifact stage
# hadolint ignore=DL3007
FROM gcr.io/distroless/base-debian12:latest
COPY --from=build /opt/build/app /usr/local/bin/pod-reaper

USER 1000
CMD ["pod-reaper"]
