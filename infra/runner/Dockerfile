# The session runner image (Unit M5.4a).
#
# Built by `make runner-image` from a binary cross-compiled on the host, so
# the image needs no Go toolchain and the binary is the one `make ci` checked.
#
# Hardening lives partly here and partly in the backend that starts it: here,
# an unprivileged user and a workspace it owns; in the backend, a read-only
# root, no capabilities and no-new-privileges. Neither half makes a container
# a security boundary — ADR-013 puts that on a microVM or gVisor provider.
FROM alpine:3.20

RUN apk add --no-cache git ca-certificates \
    && adduser -D -H -u 10001 -s /sbin/nologin runner \
    && mkdir /workspace \
    && chown 10001:10001 /workspace

COPY weave-runner /usr/local/bin/weave-runner

USER 10001:10001
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/weave-runner"]
