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
# wolfi-base provides apk, git, bash and standard utilities — minimal but usable
FROM cgr.dev/chainguard/wolfi-base:latest

# Install runtime dependencies CAS needs
RUN apk add --no-cache git bash coreutils curl

WORKDIR /app

# Copy the binary — static files are embedded
COPY --from=builder /build/cas .

EXPOSE 8080

ENTRYPOINT ["/app/cas"]
