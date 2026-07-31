# syntax=docker/dockerfile:1

# ---- Stage 1: build the React frontend ----
FROM node:20-alpine AS frontend
WORKDIR /app/frontend
COPY frontend/package.json frontend/package-lock.json* ./
RUN npm install
COPY frontend/ ./
# Vite emits into ../backend/web (see vite.config.js), so create that path.
RUN mkdir -p /app/backend/web && npm run build

# ---- Stage 2: build the Go binary (with the frontend embedded) ----
FROM golang:1.24-alpine AS backend
WORKDIR /app/backend
COPY backend/go.mod ./
# COPY go.sum if/when dependencies are added.
RUN go mod download
COPY backend/ ./
# Bring in the built frontend so //go:embed all:web includes it.
COPY --from=frontend /app/backend/web ./web
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /kube-forwarder .

# ---- Stage 3: minimal runtime with kubectl ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates curl \
    && ARCH=$(uname -m) \
    && case "$ARCH" in \
         x86_64)  KARCH=amd64 ;; \
         aarch64) KARCH=arm64 ;; \
         *) echo "unsupported arch $ARCH" && exit 1 ;; \
       esac \
    && curl -fsSLo /usr/local/bin/kubectl \
         "https://dl.k8s.io/release/v1.33.0/bin/linux/${KARCH}/kubectl" \
    && chmod +x /usr/local/bin/kubectl \
    && apk del curl

# Run as non-root.
RUN addgroup -S app && adduser -S app -G app
COPY --from=backend /kube-forwarder /usr/local/bin/kube-forwarder

USER app
# In-cluster defaults: reverse-proxy to services over cluster DNS (no
# port-forward). Listing still uses the mounted kubeconfig via kubectl.
ENV LISTEN_ADDR=0.0.0.0:8770 \
    FORWARD_MODE=proxy \
    KUBECONFIG=/etc/kube/config
EXPOSE 8770
ENTRYPOINT ["/usr/local/bin/kube-forwarder"]
