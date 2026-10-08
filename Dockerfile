FROM --platform=$TARGETPLATFORM debian:stable-slim

ARG TARGETPLATFORM

ARG SOCKET_GROUP=fireactions
ARG SOCKET_GID=1000

COPY $TARGETPLATFORM/fireactions /usr/bin/fireactions

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates                                              \
    iproute2                                                     \
    iptables                                                     \
    && groupadd --gid "$SOCKET_GID" "$SOCKET_GROUP"                 \
    && apt-get autoremove -y                                     \
    && apt-get clean                                             \
    && rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*

# This container runs the host daemon; it is not a Firecracker jailer.
# Run it as root with `--network host`, `--pid host`, and host privileges
# (for example, --privileged), and these shared host resources: /dev (including
# /dev/kvm and /dev/mapper), /run/containerd/containerd.sock, /etc/cni/net.d,
# /opt/cni/bin, /var/run/netns (rshared mount), and the configured host-local
# IPAM data directory (default: /var/lib/cni), /usr/local/bin/firecracker,
# /var/lib/fireactions/kernels, /var/lib/fireactions, and /run/fireactions.
# Mount the config file at the path passed to `fireactions server --config`.
# The daemon and reaper must share these paths and namespaces. These mounts
# and privileges provide no added jailer isolation.

COPY entrypoint.sh /usr/bin/entrypoint.sh

ENTRYPOINT ["/usr/bin/entrypoint.sh"]
