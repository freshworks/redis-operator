FROM golang:1.26.5-trixie

ENV CODEGEN_VERSION="1.27.16"

RUN apt-get update && apt-get install -y --no-install-recommends \
    bash \
    ca-certificates \
    git \
    g++ \
    openssl \
    wget \
 && rm -rf /var/lib/apt/lists/*

# Code generator stuff
# Check: https://github.com/kubernetes/kubernetes/pull/57656
RUN wget http://github.com/kubernetes/code-generator/archive/kubernetes-${CODEGEN_VERSION}.tar.gz && \
    mkdir -p /go/src/k8s.io/code-generator/ && \
    tar zxvf kubernetes-${CODEGEN_VERSION}.tar.gz --strip 1 -C /go/src/k8s.io/code-generator/ && \
    mkdir -p /go/src/k8s.io/kubernetes/hack/boilerplate/ && \
    touch /go/src/k8s.io/kubernetes/hack/boilerplate/boilerplate.go.txt

# Mock creator — assets are mockery_${VER}_Linux_{arm64|x86_64}.tar.gz (not uname -o: GNU/Linux breaks the URL)
ARG MOCKERY_VERSION="2.45.0"
RUN ARCH="$(uname -m)" && \
    case "$ARCH" in \
      x86_64|amd64) MOCKERY_ARCH=x86_64 ;; \
      aarch64|arm64) MOCKERY_ARCH=arm64 ;; \
      *) echo "mockery: unsupported arch $ARCH (need Linux arm64 or x86_64 tarball)" >&2; exit 1 ;; \
    esac && \
    wget -q -O- "https://github.com/vektra/mockery/releases/download/v${MOCKERY_VERSION}/mockery_${MOCKERY_VERSION}_Linux_${MOCKERY_ARCH}.tar.gz" | tar -xz -C /go/bin/

# Install golangci-lint
ARG GOLANGCI_LINT_VERSION="1.57.0"
RUN wget -O- -nv https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b /go/bin v${GOLANGCI_LINT_VERSION}

# Create user
ARG uid=1000
ARG gid=1000
RUN groupadd -g "$gid" rf && \
    useradd --uid "$uid" --gid rf --create-home --shell /bin/bash rf && \
    chown -R rf:rf /go


USER rf
WORKDIR /go/src/github.com/freshworks/redis-operator
