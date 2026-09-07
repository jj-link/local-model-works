FROM --platform=linux/amd64 docker:29.5.0-cli@sha256:864f36643a29d212f6cc88ec3aa1ebd2d939283df933c4c5c1074a982237f9bd AS build

RUN apk add --no-cache python3 py3-pip git bash tar gzip ca-certificates build-base libffi-dev \
    && python3 -c 'import sys; assert sys.version_info >= (3, 12)' \
    && python3 -m venv /opt/venv \
    && /opt/venv/bin/pip install --no-cache-dir --disable-pip-version-check \
        harbor==0.22.0 llm-verifier==0.2.0

FROM --platform=linux/amd64 docker:29.5.0-cli@sha256:864f36643a29d212f6cc88ec3aa1ebd2d939283df933c4c5c1074a982237f9bd

RUN apk add --no-cache python3 git bash tar gzip ca-certificates \
    && python3 -c 'import sys; assert sys.version_info >= (3, 12)'
COPY --from=build /opt/venv /opt/venv
COPY workers/benchmarks/runner.py /opt/lmw/runner.py
COPY modules/benchmarks/backend/terminal_bench_3_tasks.json /opt/lmw/terminal_bench_3_tasks.json

ENV PATH="/opt/venv/bin:${PATH}" \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    HOME=/tmp/harbor \
    TMPDIR=/tmp \
    XDG_CACHE_HOME=/tmp/harbor/.cache \
    XDG_CONFIG_HOME=/tmp/harbor/.config
WORKDIR /workspace
ENTRYPOINT ["python3", "/opt/lmw/runner.py"]
