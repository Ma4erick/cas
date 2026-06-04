# ── Build stage ───────────────────────────────────────────────────────────────
FROM cgr.dev/chainguard/go:latest AS builder

WORKDIR /build

# Cache dependencies first
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build a fully static binary
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o cas .

# ── Final stage ───────────────────────────────────────────────────────────────
FROM cgr.dev/chainguard/static:latest

WORKDIR /app

# Copy the binary — static files are embedded, nothing else needed
COPY --from=builder /build/cas .

EXPOSE 8080

ENTRYPOINT ["/app/cas"]
