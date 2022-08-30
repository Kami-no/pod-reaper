# build stage
FROM golang:1.19-alpine3.15 AS build

WORKDIR /opt/build
# hadolint ignore=DL3018
RUN apk add --no-cache git

COPY ["go.mod", "go.sum", "./"]
RUN go mod download

COPY . .
RUN GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -a -tags "netgo" -ldflags '-s -w' -o pod-reaper

# artefact stage
FROM alpine:3.15
COPY --from=build /opt/build/pod-reaper /usr/local/bin/pod-reaper
CMD ["pod-reaper"]
