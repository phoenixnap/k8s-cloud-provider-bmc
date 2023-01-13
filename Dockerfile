# syntax=docker/dockerfile:1.1-experimental

# Build the manager binary
ARG GOVER=1.19.1
FROM --platform=$BUILDPLATFORM golang:${GOVER} as builder

ARG TARGETPLATFORM
ARG BUILDPLATFORM

WORKDIR /workspace

# Run this with docker build --build_arg $(go env GOPROXY) to override the goproxy
ARG goproxy=https://proxy.golang.org
ENV GOPROXY=$goproxy

COPY go.mod .
COPY go.sum .
RUN go mod download

COPY . .

# Build
ARG TARGETARCH
ARG LDFLAGS
ARG BINARY=cloud-provider-phoenixnap
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -a -ldflags "${LDFLAGS} -extldflags '-static'" \
    -o "${BINARY}" .

# because you cannot use ARG or ENV in CMD when in [] mode

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
WORKDIR /
ARG BINARY=cloud-provider-phoenixnap
COPY --from=builder /workspace/${BINARY} ./cloud-provider-phoenixnap
USER nonroot:nonroot
ENTRYPOINT ["./cloud-provider-phoenixnap"]
