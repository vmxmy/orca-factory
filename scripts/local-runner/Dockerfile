FROM ghcr.io/actions/actions-runner@sha256:5036480998280bb21e32ade9fe1b02b493861ac314b62ba1aea320b94f56ec97
USER root
RUN apt-get update && apt-get install -y --no-install-recommends bash git make gcc libc6-dev ca-certificates curl unzip && rm -rf /var/lib/apt/lists/*
COPY --chmod=0755 entrypoint.sh /usr/local/bin/orca-runner-entrypoint
ENV RUNNER_TOOL_CACHE=/home/runner/_work/_tool AGENT_TOOLSDIRECTORY=/home/runner/_work/_tool
ENV GOPATH=/home/runner/_work/go GOCACHE=/home/runner/_work/go-build-cache XDG_CACHE_HOME=/home/runner/_work/.cache
USER 1001:123
WORKDIR /home/runner
ENTRYPOINT ["/usr/local/bin/orca-runner-entrypoint"]
