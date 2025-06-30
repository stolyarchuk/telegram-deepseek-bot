# Build stage
FROM golang:1.24-bookworm AS builder

WORKDIR /app

# First copy only dependency files for better layer caching
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

# Copy the rest of the application
COPY . .

# Build the application (static binary required for distroless)
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 GOOS=linux go build -ldflags="-s -w" -o telegram-deepseek-bot main.go

# Runtime stage - using Google distroless base debian with nonroot user for glibc linking
FROM gcr.io/distroless/base-debian12:nonroot

# Copy only necessary files from builder
COPY --from=builder --chown=nonroot /app/telegram-deepseek-bot /telegram-deepseek-bot
COPY --from=builder --chown=nonroot /app/conf /conf

USER nonroot:nonroot
WORKDIR /

# Runtime command
CMD ["/telegram-deepseek-bot"]
