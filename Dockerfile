# base golang image
ARG GOVER="1.13.8"
FROM golang:${GOVER} as golang

ARG REPO

RUN apt-get update -y && apt-get install -y git ca-certificates

RUN GO111MODULE=off go get -u golang.org/x/lint/golint

ENV GO111MODULE=on 
ENV CGO_ENABLED=0

WORKDIR /go/src/${REPO}

COPY go.mod .
COPY go.sum .
RUN go mod download
COPY . .

# these have to be last steps so they do not bust the cache with each change
ARG OS
ARG ARCH
ENV GOOS=${OS} 
ENV GOARCH=${ARCH} 

# builder
FROM golang as build

RUN go build -v -i -o /usr/local/bin/node-detacher

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
WORKDIR /

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /usr/local/bin/node-detacher /manager

USER nonroot:nonroot

ENTRYPOINT ["/manager"]
