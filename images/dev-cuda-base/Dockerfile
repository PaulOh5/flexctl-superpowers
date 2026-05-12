FROM nvidia/cuda:12.4.1-base-ubuntu22.04
RUN apt-get update && apt-get install -y --no-install-recommends \
    openssh-server sudo python3 python3-pip vim curl ca-certificates \
 && rm -rf /var/lib/apt/lists/*
RUN useradd -m -s /bin/bash dev \
 && mkdir -p /home/dev/.ssh && chown -R dev:dev /home/dev/.ssh && chmod 700 /home/dev/.ssh \
 && mkdir /run/sshd
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
EXPOSE 22
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
