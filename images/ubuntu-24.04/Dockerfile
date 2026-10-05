FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive
ARG TARGETARCH

RUN apt-get update \
    && apt-get install -y --no-install-recommends systemd systemd-sysv bash coreutils git ca-certificates curl \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* \
    && if account="$(getent passwd 1000)"; then userdel --remove "${account%%:*}"; fi \
    && if group="$(getent group 1000)"; then groupdel "${group%%:*}"; fi \
    && groupadd --gid 1000 ci \
    && useradd --uid 1000 --gid 1000 --create-home --home-dir /home/ci --shell /bin/bash ci \
    && install -d -o ci -g ci /workspace \
    && case "$TARGETARCH" in amd64) node_arch=x64; node_sha=6e1db87ef58b8819e5d5402eff1536491b18edd8eb7bee5ef7897876e88dc5ff ;; arm64) node_arch=arm64; node_sha=724282c3b43aec998aa9527380465b45d229e021b58035f5f4f63095eabfe5d5 ;; *) echo "unsupported TARGETARCH: $TARGETARCH" >&2; exit 1 ;; esac \
    && node_file="node-v24.21.0-linux-${node_arch}.tar.gz" \
    && curl -fsSLo /tmp/SHASUMS256.txt https://nodejs.org/dist/v24.21.0/SHASUMS256.txt \
    && grep -Fx "$node_sha  $node_file" /tmp/SHASUMS256.txt \
    && curl -fsSLo /tmp/node.tar.gz "https://nodejs.org/dist/v24.21.0/${node_file}" \
    && echo "$node_sha  /tmp/node.tar.gz" | sha256sum -c - \
    && tar -xzf /tmp/node.tar.gz -C /usr/local --strip-components=1 --no-same-owner \
    && rm /tmp/node.tar.gz /tmp/SHASUMS256.txt \
    && ln -sf /bin/true /usr/sbin/policy-rc.d

COPY fireactions /usr/local/bin/fireactions
COPY images/ubuntu-24.04/fireactions-agent.service /etc/systemd/system/fireactions-agent.service

RUN chmod 0755 /usr/local/bin/fireactions \
    && systemctl enable fireactions-agent.service \
    && systemctl mask systemd-networkd.service systemd-networkd.socket systemd-resolved.service NetworkManager.service ssh.service sshd.service getty@.service serial-getty@.service console-getty.service getty.target
RUN rm -rf /etc/netplan /etc/systemd/network /etc/ssh /var/lib/ssh /root/.ssh /home/ci/.ssh \
    && rm -f /etc/machine-id /var/lib/dbus/machine-id \
    && install -d /var/lib/dbus \
    && touch /etc/machine-id \
    && ln -s /etc/machine-id /var/lib/dbus/machine-id

STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
