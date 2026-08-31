# Layered on the base session image rather than repeating it, so a change to how
# sessions are built reaches every profile. Build base first: `make image`.
ARG BASE=ccvm/base:latest
FROM ${BASE}

ARG TARGETARCH
# Debian's golang lags, and a session for Go work is the one place that matters,
# so this takes the upstream tarball rather than the distro package.
ARG GO_VERSION=1.27.0
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" \
      | tar -C /usr/local -xz \
 && ln -sf /usr/local/go/bin/go /usr/local/bin/go \
 && ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt

ENV GOPATH=/root/go
ENV PATH=/usr/local/go/bin:/root/go/bin:$PATH

RUN go install github.com/go-delve/delve/cmd/dlv@latest \
 && go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest

WORKDIR /work
