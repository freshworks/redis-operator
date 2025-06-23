#!/usr/bin/env bash

set -o errexit
set -o nounset

src=./cmd/redisoperator
out=./bin/redis-operator

# Set default values if not defined
TARGETOS=${TARGETOS:-$(go env GOOS)}
TARGETARCH=${TARGETARCH:-$(go env GOARCH)}
binary_ext=""

if [[ ! -z ${TARGETOS} ]] && [[ ! -z ${TARGETARCH} ]];
then
    echo "Building ${TARGETOS}/${TARGETARCH} release..."
    export GOOS=${TARGETOS}
    export GOARCH=${TARGETARCH}
    binary_ext=-${TARGETOS}-${TARGETARCH}
else
    echo "Building native release..."
fi

final_out=${out}${binary_ext}
ldf_cmp="-w -extldflags '-static'"
f_ver="-X main.Version=${VERSION:-dev}"

echo "Building binary at ${out}"
CGO_ENABLED=0 go build -o ${out} --ldflags "${ldf_cmp} ${f_ver}"  ${src}
