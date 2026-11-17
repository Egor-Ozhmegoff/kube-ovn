FROM ubuntu:26.04
ENV DEBIAN_FRONTEND=noninteractive
RUN printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d && chmod +x /usr/sbin/policy-rc.d && apt-get update && apt-get install -y --no-install-recommends make gcc libc6-dev git ca-certificates curl tcpdump socat netcat-openbsd iputils-ping iputils-tracepath iproute2 iptables python3-yaml python3-jsonschema podman systemd openvswitch-switch ovn-central ovn-host && rm -rf /var/lib/apt/lists/*
ENTRYPOINT ["/bin/bash"]
