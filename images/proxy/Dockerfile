# The egress proxy of agent runs (CSR-63): a static Go binary built from
# main.go with the standard library only, on an empty base, run as nobody.
FROM golang:1.27.3-bookworm AS build
WORKDIR /src
COPY main.go .
RUN CGO_ENABLED=0 GOFLAGS=-trimpath go build -ldflags=-s -o /proxy main.go

FROM scratch
COPY --from=build /proxy /proxy
USER 65534:65534
EXPOSE 3128
ENTRYPOINT ["/proxy"]
