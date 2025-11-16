# Build stage
FROM golang:1.25.4-trixie AS builder

# Set working directory
WORKDIR /app

# Install necessary tools
RUN apt-get update && apt-get install -y --no-install-recommends \
    git \
    ca-certificates \
    build-essential \
    && rm -rf /var/lib/apt/lists/*

# Download Go modules
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build \
    -ldflags='-w -s' \
    -o nvidia-gpu-exporter \
    ./cmd/nvidia-gpu-exporter

# Runtime stage
FROM nvidia/dcgm:4.4.1-2-ubuntu22.04

# Set labels
LABEL maintainer="nvidia-gpu-exporter"
LABEL description="NVIDIA GPU Exporter"
LABEL version="1.0.0"

# Install necessary packages
RUN apt-get update && apt-get install -y \
    ca-certificates \
    curl \
    && rm -rf /var/lib/apt/lists/*

# Create non-root user
RUN groupadd -r exporter && useradd -r -g exporter exporter

# Create working directory
WORKDIR /app

# Copy built binary
COPY --from=builder /app/nvidia-gpu-exporter ./

# Set execution permissions for binary
RUN chmod +x nvidia-gpu-exporter

# Change user
USER exporter

# Expose port
EXPOSE 9432

# Entry point
ENTRYPOINT ["/app/nvidia-gpu-exporter"]

# Default arguments
CMD [] 
