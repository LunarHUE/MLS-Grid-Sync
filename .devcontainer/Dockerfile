FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    bash \
    bash-completion \
    ca-certificates \
    curl \
    direnv \
    git \
    git-lfs \
    gnupg \
    locales \
    openssh-client \
    sudo \
    xz-utils \
    && rm -rf /var/lib/apt/lists/*

RUN sed -i '/en_US.UTF-8/s/^# //g' /etc/locale.gen && locale-gen
ENV LANG=en_US.UTF-8
ENV LC_ALL=en_US.UTF-8

# Single-user Nix install as root — no daemon needed.
RUN curl --proto '=https' --tlsv1.2 -sSf -L \
    https://install.determinate.systems/nix | sh -s -- install linux \
    --no-confirm --init none --extra-conf "filter-syscalls = false" \
&& sed -i '/install\.determinate\.systems/d' /etc/nix/nix.conf

ENV PATH="/nix/var/nix/profiles/default/bin:/root/.nix-profile/bin:${PATH}"
ENV NIX_CONFIG="experimental-features = nix-command flakes"

WORKDIR /workspace
