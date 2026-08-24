# syntax=docker/dockerfile:1.7
FROM node:22.22.0-bookworm-slim@sha256:dd9d21971ec4395903fa6143c2b9267d048ae01ca6d3ea96f16cb30df6187d94 AS node

FROM ubuntu:24.04@sha256:33ceb71981b602c1a7443a53469e4dba065f7503eab3078a2d7a57a2ab987517
COPY --from=node /usr/local/ /usr/local/

ARG CLAUDE_CODE_VERSION=2.1.241
ARG CODEX_VERSION=0.149.1
ARG UV_VERSION=0.12.5

ENV DEBIAN_FRONTEND=noninteractive \
    PATH=/usr/local/bin:/usr/local/sbin:/usr/bin:/usr/sbin:/bin:/sbin \
    CLAUDE_PLUGIN_ROOT=/opt/agon \
    AGON_RUNNER=/usr/local/bin/lmw-agon-runner \
    HOME=/home/agon

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates git openssh-client python3 python3-pip \
      latexmk biber texlive-latex-base texlive-latex-extra texlive-fonts-recommended \
      texlive-bibtex-extra poppler-utils tini \
 && npm install --global "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" "@openai/codex@${CODEX_VERSION}" \
 && python3 -m pip install --break-system-packages --no-cache-dir "uv==${UV_VERSION}" \
 && apt-get clean \
 && rm -rf /var/lib/apt/lists/* /root/.cache /root/.npm

RUN addgroup --gid 10001 agon \
 && adduser --uid 10001 --gid 10001 --disabled-password --gecos "" --home /home/agon --shell /bin/bash agon \
 && install -d -o agon -g agon -m 0700 /home/agon/.claude /home/agon/.codex

COPY --chown=root:root third_party/agon/ /opt/agon/
COPY --chown=root:root .build/worker/lmw-agon-runner /usr/local/bin/lmw-agon-runner
RUN chmod 0555 /usr/local/bin/lmw-agon-runner \
 && find /opt/agon -type d -exec chmod 0555 {} + \
 && find /opt/agon -type f -exec chmod 0444 {} +

USER 10001:10001
WORKDIR /project/artifacts
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/lmw-agon-runner"]
