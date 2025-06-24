FROM --platform=$BUILDPLATFORM golang:1.24-bullseye AS build
# Debian already includes bash

WORKDIR /src
COPY . .

ARG TARGETOS 
ARG TARGETARCH 
ARG VERSION
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH VERSION=$VERSION ./scripts/build.sh

# Use distroless as minimal base image
# https://github.com/GoogleContainerTools/distroless
FROM gcr.io/distroless/static-debian12:nonroot AS final
WORKDIR /

# Copy the binary from the build stage
COPY --from=build /src/bin/redis-operator /usr/local/bin/redis-operator

# Use nonroot user (provided by distroless)
USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/redis-operator"]
