# The reaper runs `ccvm gc` inside the cluster, so it needs the ccvm binary and
# kubectl. Deliberately not the session image: that one is what Claude runs in,
# and it has no reason to carry a Kubernetes client or credentials for one.
FROM debian:trixie-slim

ARG TARGETARCH

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*

# Pinned to the cluster-supported minor rather than "stable", so a reaper does
# not silently change client version between builds.
ARG KUBECTL_VERSION=v1.34.1
RUN curl -fsSLo /usr/local/bin/kubectl \
      "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" \
 && chmod 0755 /usr/local/bin/kubectl

COPY dist/ccvm-linux-${TARGETARCH} /usr/local/bin/ccvm
RUN chmod 0755 /usr/local/bin/ccvm

ENTRYPOINT ["/usr/local/bin/ccvm"]
