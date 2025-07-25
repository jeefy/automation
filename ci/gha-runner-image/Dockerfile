ARG BASE_IMAGE

FROM --platform=$BUILDPLATFORM mcr.microsoft.com/dotnet/runtime-deps:8.0-${BASE_IMAGE} AS build

ARG RUNNER_VERSION=2.326.0
ARG RUNNER_CONTAINER_HOOKS_VERSION=0.7.0
ARG DOCKER_VERSION=28.0.4
ARG BUILDX_VERSION=0.22.0
ARG TARGETOS
ARG TARGETARCH

RUN apt update -y \
    && apt install curl unzip -y \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /actions-runner
RUN export RUNNER_ARCH=${TARGETARCH} \
    && if [ "$RUNNER_ARCH" = "amd64" ]; then export RUNNER_ARCH=x64 ; fi \
    && echo "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-${TARGETOS}-${RUNNER_ARCH}-${RUNNER_VERSION}.tar.gz" \
    && curl -f -L -o runner.tar.gz https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-${TARGETOS}-${RUNNER_ARCH}-${RUNNER_VERSION}.tar.gz \
    && tar xzf ./runner.tar.gz \
    && rm runner.tar.gz

RUN curl -f -L -o runner-container-hooks.zip https://github.com/actions/runner-container-hooks/releases/download/v${RUNNER_CONTAINER_HOOKS_VERSION}/actions-runner-hooks-k8s-${RUNNER_CONTAINER_HOOKS_VERSION}.zip \
    && unzip ./runner-container-hooks.zip -d ./k8s \
    && rm runner-container-hooks.zip

RUN export RUNNER_ARCH=${TARGETARCH} \
    && if [ "$RUNNER_ARCH" = "amd64" ]; then export DOCKER_ARCH=x86_64 ; fi \
    && if [ "$RUNNER_ARCH" = "arm64" ]; then export DOCKER_ARCH=aarch64 ; fi \
    && curl -fLo docker.tgz https://download.docker.com/${TARGETOS}/static/stable/${DOCKER_ARCH}/docker-${DOCKER_VERSION}.tgz \
    && tar zxvf docker.tgz \
    && rm -rf docker.tgz \
    && mkdir -p /usr/local/lib/docker/cli-plugins \
    && curl -fLo /usr/local/lib/docker/cli-plugins/docker-buildx \
        "https://github.com/docker/buildx/releases/download/v${BUILDX_VERSION}/buildx-v${BUILDX_VERSION}.linux-${TARGETARCH}" \
    && chmod +x /usr/local/lib/docker/cli-plugins/docker-buildx

FROM mcr.microsoft.com/dotnet/runtime-deps:8.0-${BASE_IMAGE}

ARG TARGETOS
ARG TARGETARCH

ENV DEBIAN_FRONTEND=noninteractive
ENV RUNNER_MANUALLY_TRAP_SIG=1
ENV ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT=1
ENV PIP_BREAK_SYSTEM_PACKAGES=1

# 'gpg-agent' and 'software-properties-common' are needed for the 'add-apt-repository' command that follows
RUN apt update -y \
    && apt upgrade -y \
    && apt-get install -y --no-install-recommends \
            curl \
            lsb-release \
            jq \
            software-properties-common \
            sudo \
            unzip \
            zlib1g \
            kmod \
            zstd \
            dirmngr \
            wget \
            gnupg \
            python3-pip \
            locales \
            make \
            eatmydata \
            maven \
            zip \
            xz-utils \
            file \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*



RUN locale-gen en_US.UTF-8 \
    && echo "LANG=en_US.UTF-8" >> /etc/default/locale \
    && echo "LC_ALL=en_US.UTF-8" >> /etc/default/locale \
    && echo "LANGUAGE=en_US.UTF-8" >> /etc/default/locale

# Configure git-core/ppa based on guidance here:  https://git-scm.com/download/linux
RUN add-apt-repository ppa:git-core/ppa \
    && apt update -y \
    && apt install -y git \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

RUN adduser --disabled-password --gecos "" --uid 1001 runner \
    && groupadd docker --gid 123 \
    && usermod -aG sudo runner \
    && usermod -aG docker runner \
    && echo "%sudo   ALL=(ALL:ALL) NOPASSWD:ALL" > /etc/sudoers \
    && echo "Defaults env_keep += \"DEBIAN_FRONTEND\"" >> /etc/sudoers

WORKDIR /home/runner

COPY --chown=runner:docker --from=build /actions-runner .
COPY --from=build /usr/local/lib/docker/cli-plugins/docker-buildx /usr/local/lib/docker/cli-plugins/docker-buildx

RUN install -o root -g root -m 755 docker/* /usr/bin/ && rm -rf docker

RUN curl -LO https://go.dev/dl/go1.24.2.linux-${TARGETARCH}.tar.gz \
    && tar -C /usr/local -xzf go1.24.2.linux-${TARGETARCH}.tar.gz \
    && rm go1.24.2.linux-${TARGETARCH}.tar.gz \
    && echo 'export PATH=$PATH:/usr/local/go/bin:/home/runner/go/bin:/home/runner/.local/bin' >> /etc/profile \
    && echo 'export PATH=$PATH:/usr/local/go/bin:/home/runner/go/bin:/home/runner/.local/bin' >> /home/runner/.profile \
    && echo 'export PATH=$PATH:/usr/local/go/bin:/home/runner/go/bin:/home/runner/.local/bin' >> /home/runner/.bashrc

ENV ENV=/etc/profile

USER runner

RUN pip3 install --user launchable~=1.0

RUN export PATH=$PATH:/usr/local/go/bin:/home/runner/go/bin && \
    go install sigs.k8s.io/kind@v0.27.0
