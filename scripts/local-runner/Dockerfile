FROM ghcr.io/actions/actions-runner@sha256:660f7b9d1e0007274f7c867e220b3c382137ef5a30d8e501a025ad50dbd5fb9d
USER root
RUN sed -i -E 's#https?://(archive|security).ubuntu.com/ubuntu#https://mirrors.aliyun.com/ubuntu#g' /etc/apt/sources.list.d/ubuntu.sources && apt-get update && apt-get install -y --no-install-recommends bash git make gcc libc6-dev ca-certificates curl unzip && rm -rf /var/lib/apt/lists/*
ARG GO_VERSION
ARG GO_SHA256
ARG GO_DOWNLOAD_BASE_URL=https://mirrors.aliyun.com/golang
# The tool cache and module cache live outside the per-job _work tmpfs so every job starts with them.
ENV RUNNER_TOOL_CACHE=/opt/hostedtoolcache AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache GOPATH=/opt/orca-go
# setup-go accepts go/<version>/x64 with an x64.complete marker as a cache hit and skips its download.
RUN test -n "$GO_VERSION" && test -n "$GO_SHA256" \
 && curl -fsSL -o /tmp/go.tgz "$GO_DOWNLOAD_BASE_URL/go$GO_VERSION.linux-amd64.tar.gz" \
 && echo "$GO_SHA256  /tmp/go.tgz" | sha256sum -c - \
 && mkdir -p "$RUNNER_TOOL_CACHE/go/$GO_VERSION/x64" "$GOPATH" \
 && tar -xzf /tmp/go.tgz -C "$RUNNER_TOOL_CACHE/go/$GO_VERSION/x64" --strip-components=1 \
 && touch "$RUNNER_TOOL_CACHE/go/$GO_VERSION/x64.complete" \
 && rm /tmp/go.tgz && chown -R 1001:123 "$RUNNER_TOOL_CACHE" "$GOPATH"
COPY --chmod=0755 entrypoint.sh /usr/local/bin/orca-runner-entrypoint
USER 1001:123
WORKDIR /home/runner
ARG GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct
COPY --chown=1001:123 prewarm/ /tmp/orca-src/
# Go trims build-cache entries unused for five days, so only modules are kept; compile output is discarded.
RUN export PATH="$RUNNER_TOOL_CACHE/go/$GO_VERSION/x64/bin:$PATH" GOCACHE=/tmp/prewarm-build-cache \
 && (cd /tmp/orca-src/driver && go mod download) \
 && /tmp/orca-src/scripts/install-dev-tools.sh gotestsum golangci-lint govulncheck actionlint \
 && rm -rf /tmp/orca-src /tmp/prewarm-build-cache
ENV GOCACHE=/home/runner/_work/go-build-cache XDG_CACHE_HOME=/home/runner/_work/.cache
ENTRYPOINT ["/usr/local/bin/orca-runner-entrypoint"]
