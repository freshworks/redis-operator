FROM golang:1.24-alpine

ENV CODEGEN_VERSION="1.27.16"

RUN apk --no-cache add \
    bash \
    git \
    g++ \
    openssl

# Code generator stuff
# Check: https://github.com/kubernetes/kubernetes/pull/57656
RUN wget http://github.com/kubernetes/code-generator/archive/kubernetes-${CODEGEN_VERSION}.tar.gz && \
    mkdir -p /go/src/k8s.io/code-generator/ && \
    tar zxvf kubernetes-${CODEGEN_VERSION}.tar.gz --strip 1 -C /go/src/k8s.io/code-generator/ && \
    mkdir -p /go/src/k8s.io/kubernetes/hack/boilerplate/ && \
    touch /go/src/k8s.io/kubernetes/hack/boilerplate/boilerplate.go.txt

# Mock creator
ARG MOCKERY_VERSION="2.45.0"
RUN wget -c https://github.com/vektra/mockery/releases/download/v${MOCKERY_VERSION}/mockery_${MOCKERY_VERSION}_$(uname -o)_$(uname -m).tar.gz -O - | tar -xz -C /go/bin/

# Install golangci-lint
ARG GOLANGCI_LINT_VERSION="1.57.0"
RUN wget -O- -nv https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b /go/bin v${GOLANGCI_LINT_VERSION}

# Create user
ARG uid=1000
ARG gid=1000
RUN addgroup -g $gid rf && \
    adduser -D -u $uid -G rf rf && \
    chown rf:rf -R /go


USER rf
WORKDIR /go/src/github.com/freshworks/redis-operator
