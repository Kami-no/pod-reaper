# build stage
FROM golang:1.22-bookworm AS build

WORKDIR /opt/build

COPY ["go.mod", "go.sum", "./"]
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -a -tags "netgo" -ldflags '-s -w' -o app

# artifact stage
# hadolint ignore=DL3007
FROM gcr.io/distroless/base-debian12:latest
WORKDIR /opt/app

COPY --from=build /opt/build/app /usr/local/bin/pod-reaper
CMD ["pod-reaper"]
