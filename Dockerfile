# Build image
FROM --platform=$BUILDPLATFORM golang:1.21.0 AS build
ARG BUILDPLATFORM
ARG TARGETPLATFORM
# https://github.com/docker/buildx/issues/510#issuecomment-768432329
ENV BUILDPLATFORM=${BUILDPLATFORM:-linux/amd64}
ENV TARGETPLATFORM=${TARGETPLATFORM:-linux/amd64}

LABEL org.opencontainers.image.source="https://github.com/gubernator-io/gubernator"

WORKDIR /go/src

# This should create cached layer of our dependencies for subsequent builds to use
COPY go.mod /go/src
COPY go.sum /go/src
RUN go mod download

# Copy the local package files to the container
ADD . /go/src

ARG VERSION

# Build the server inside the container
RUN CGO_ENABLED=0 GOOS=${TARGETPLATFORM%/*} GOARCH=${TARGETPLATFORM#*/} go build \
    -ldflags "-w -s -X main.Version=$VERSION" -o /gubernator /go/src/cmd/gubernator/main.go

RUN CGO_ENABLED=0 GOOS=${TARGETPLATFORM%/*} GOARCH=${TARGETPLATFORM#*/} go build \
    -ldflags "-w -s" -o /healthcheck /go/src/cmd/healthcheck/main.go

# Create a non-root user
RUN useradd -u 1001 gubernator

# Create our deploy image
FROM scratch

# Certs for ssl
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy our static executable.
COPY --from=build /gubernator /gubernator
COPY --from=build /healthcheck /healthcheck

# Switch to non root user
USER 1001

# Healtcheck
HEALTHCHECK --interval=3s --timeout=1s --start-period=2s --retries=2 CMD [ "/healthcheck" ]

# Run the server
ENTRYPOINT ["/gubernator"]

EXPOSE 1050
EXPOSE 1051
EXPOSE 7946
