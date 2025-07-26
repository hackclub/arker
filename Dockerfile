FROM golang:1.24-bookworm

# Install required packages and Playwright dependencies
RUN apt-get update && apt-get install -y \
    git \
    python3 \
    python3-pip \
    curl \
    ca-certificates \
    ffmpeg \
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
    && rm -rf /var/lib/apt/lists/*

# Install yt-dlp
RUN pip3 install --break-system-packages yt-dlp[default]

WORKDIR /app

# Copy source and build application
COPY . .
# Build the application
RUN go build -o arker ./cmd

# Install Playwright CLI that matches our library version
RUN go install github.com/playwright-community/playwright-go/cmd/playwright@v0.4501.1
ENV PATH="/root/go/bin:${PATH}"

# Ensure Playwright driver is installed
RUN playwright install-deps
RUN playwright install --with-deps chromium

# Verify installation
RUN playwright --version

# Create necessary directories
RUN mkdir -p /data /cache

# Expose port
EXPOSE 8080

CMD ["./arker"]
