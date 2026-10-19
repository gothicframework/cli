# Go build stage
FROM golang:1.26-bookworm AS builder

WORKDIR /src
COPY . .

# Build the Go application
RUN CGO_ENABLED=0 GOOS=linux go build -o /app .

# Final stage
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=builder /app /app

# Cloud Run injects the port via the PORT environment variable.
# The app reads HTTP_LISTEN_ADDR from config; when PORT is set,
# the runtime resolves it to bind to that port.
EXPOSE 8080
ENV PORT=8080

CMD ["/app"]
