FROM golang:1.24-bookworm

# Install system dependencies in a single layer with aggressive cleanup
RUN apt-get update && apt-get install -y --no-install-recommends \
    git \
    python3 \
    python3-pip \
    curl \
    ca-certificates \
    ffmpeg \
    aria2 \
    unzip \
    # tini is used as PID 1 (see ENTRYPOINT) to reap orphaned browser subprocesses
    tini \
    # Playwright browser dependencies
    libnss3 \
    libnspr4 \
    libatk1.0-0 \
    libatk-bridge2.0-0 \
    libcups2 \
    libdbus-1-3 \
    libdrm2 \
    libxkbcommon0 \
    libxcomposite1 \
    libxdamage1 \
    libxext6 \
    libxfixes3 \
    libxrandr2 \
    libgbm1 \
    libpango-1.0-0 \
    libcairo2 \
    libasound2 \
    libatspi2.0-0 \
    libgtk-3-0 \
    libxss1 \
    fonts-liberation \
    libappindicator3-1 \
    lsb-release \
    xdg-utils \
    wget \
    vainfo \
    mesa-va-drivers \
    va-driver-all \
    libva2 \
    libva-drm2 \
    && pip3 install --break-system-packages --no-cache-dir itch-dl \
    && apt-get autoremove -y \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/* /root/.cache

# yt-dlp is intentionally installed from the nightly (--pre) channel because
# Instagram breaks extractors faster than stable releases ship. This remote ADD
# invalidates Docker's cache whenever the latest nightly release changes, so a
# Coolify rebuild actually refreshes yt-dlp instead of reusing a stale layer.
ADD https://api.github.com/repos/yt-dlp/yt-dlp-nightly-builds/releases/latest /tmp/yt-dlp-nightly-release.json
RUN pip3 install --break-system-packages --no-cache-dir --upgrade --pre "yt-dlp[default,curl-cffi]" \
    && yt-dlp --version > /etc/yt-dlp-version \
    && rm -rf /root/.cache

# The Docker image includes curl-cffi, so use yt-dlp's browser impersonation by
# default for Instagram/TikTok/Facebook anti-bot responses. Arker applies this
# only to those URL families. Override to empty to disable, or to another target
# accepted by `yt-dlp --list-impersonate-targets`.
ENV YTDLP_IMPERSONATE=chrome

# Install Deno
RUN curl -fsSL https://deno.land/install.sh | sh
ENV DENO_INSTALL="/root/.deno"
ENV PATH="${DENO_INSTALL}/bin:${PATH}"

# Install Playwright CLI early for better caching
RUN go install github.com/mxschmitt/playwright-go/cmd/playwright@v0.6100.0
ENV PATH="/root/go/bin:${PATH}"

# Install Playwright browser in separate layer (cached unless Playwright version changes)
RUN playwright install-deps && \
    playwright install --with-deps chromium && \
    playwright --version

WORKDIR /app

# Copy go.mod and go.sum first for dependency caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code and build (this layer rebuilds on code changes)
COPY . .
RUN go build -o arker ./cmd

# Create necessary directories
RUN mkdir -p /data /cache

EXPOSE 8080

# Run under tini as PID 1. Playwright's chromium/headless_shell children get
# reparented to PID 1 when their launcher exits; arker does not reap them, so they
# accumulated as zombies and eventually exhausted the container PID limit. tini
# reaps them automatically, and this is baked into the image so it holds regardless
# of whether the runtime sets `docker run --init`.
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["./arker"]
