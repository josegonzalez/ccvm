# Layered on the base session image rather than repeating it, so a change to how
# sessions are built reaches every profile. Build base first: `make image`.
ARG BASE=ccvm/base:latest
FROM ${BASE}

# NodeSource rather than the distro package, which ships a version old enough to
# fail on most lockfiles.
ARG NODE_MAJOR=24
RUN curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | bash - \
 && apt-get install -y --no-install-recommends nodejs \
 && rm -rf /var/lib/apt/lists/*

RUN corepack enable \
 && npm install -g typescript

WORKDIR /work
