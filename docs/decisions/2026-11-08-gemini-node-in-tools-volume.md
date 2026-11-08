# Node for Gemini CLI in the tools volume

- **Status:** Accepted
- **Date:** 2026-11-08
- **Source:** Decided by Willem on CSR-57.

## Decision

Gemini CLI (0.67.0) ships for Linux only as the npm package `@google/gemini-cli`: one self-contained JS bundle that needs Node >= 20. The tools volume carries both:

- the npm tarball, checked by its npm sha512 integrity, unpacked into `gemini/`;
- Node 22.23.4 in `bin/node`: on glibc images the official nodejs.org `linux-<arch>.tar.gz`, checked by the SHA-256 from its `SHASUMS256.txt`; on musl images the `node` binary of the `node:22-alpine` image that is already pinned by digest for OpenCode's musl libraries, with those libraries.

## Reason

Gemini then works on any agent image, as the other three clients do, without asking each project's image for Node. On musl no new pin is needed. The `.tar.gz` (not `.tar.xz`) build is used because Go's standard library reads gzip. Rejected: copying Node from digest-pinned images for glibc too (an extra ~200 MB pull for one binary), requiring Node in the agent image (fails at run time on images without it), leaving Gemini out of M7.
