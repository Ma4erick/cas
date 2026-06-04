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
# Use git image — includes git, sh and CA certs needed for CAS git operations
FROM cgr.dev/chainguard/git:latest

WORKDIR /app

# Copy the binary — static files are embedded
COPY --from=builder /build/cas .

EXPOSE 8080

ENTRYPOINT ["/app/cas"]
