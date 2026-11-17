# Example agent image for a Node project in the docker environment: copy
# this file and compose.yml into the project's .cinderline/. Agent CLIs come
# from the runner's tools volume; the runner runs the image as uid 1000:1000,
# which is the image's own user node, home /home/node.
FROM node:22-bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/*

USER node
