FROM alpine:3.20
RUN apk add --no-cache iptables ip6tables ca-certificates wget tar
ARG TS_VERSION=1.76.6
RUN wget -q https://pkgs.tailscale.com/stable/tailscale_${TS_VERSION}_amd64.tgz \
 && tar xzf tailscale_${TS_VERSION}_amd64.tgz \
 && mv tailscale_${TS_VERSION}_amd64/tailscaled /usr/local/bin/ \
 && mv tailscale_${TS_VERSION}_amd64/tailscale  /usr/local/bin/ \
 && rm -rf tailscale_${TS_VERSION}_amd64*
COPY bin/flexctl /usr/local/bin/flexctl
RUN chmod +x /usr/local/bin/flexctl
ENTRYPOINT ["/usr/local/bin/flexctl", "sidecar"]
