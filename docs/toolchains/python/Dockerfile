# Example agent image for a Python project in the docker environment: copy
# this file and compose.yml into the project's .cinderline/. Agent CLIs come
# from the runner's tools volume; the runner runs the image as uid 1000:1000.
FROM python:3.13-slim-bookworm

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 1000 agent

# A virtual environment uid 1000 owns, so the agent can add packages in a turn
# (with pypi.org:443 and files.pythonhosted.org:443 in the project's allow_hosts).
USER agent
RUN python -m venv /home/agent/venv \
    && /home/agent/venv/bin/pip install --no-cache-dir pytest ruff
ENV PATH=/home/agent/venv/bin:$PATH
