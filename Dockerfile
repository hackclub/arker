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
    && pip3 install --break-system-packages --no-cache-dir yt-dlp[default] \
    && apt-get autoremove -y \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/* /root/.cache

# Install Playwright CLI early for better caching
RUN go install github.com/playwright-community/playwright-go/cmd/playwright@v0.4501.1
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

CMD ["./arker"]
