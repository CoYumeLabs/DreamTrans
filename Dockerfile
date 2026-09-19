# Builder stages run on the build machine's own architecture
# (--platform=$BUILDPLATFORM) and cross-compile for the target. Without this a
# linux/arm64 image build runs npm, Vite and the Go compiler under QEMU
# emulation, which is the slowest step of the whole CI pipeline by far. Only
# the final runtime stage below executes on the target platform.

# ---- Stage 1: Build Frontend ----
# Node 24 "Krypton" is the current LTS line. Pin both Node and Alpine so image
# rebuilds cannot silently change the frontend toolchain.
FROM --platform=$BUILDPLATFORM node:24.18.0-alpine3.23 AS frontend-builder

# Set working directory
WORKDIR /app/frontend

# Copy package.json and package-lock.json
COPY frontend/package*.json ./

# Install dependencies
RUN npm ci

# Copy the rest of the frontend source code
COPY frontend/ ./

# Build the frontend for production
# These build args will be used as environment variables during build
ARG VITE_BACKEND_URL=/
ARG VITE_BACKEND_WS_URL=/
ARG VITE_APP_COMMIT
ARG VITE_APP_BUILD_TIME
ENV VITE_BACKEND_URL=$VITE_BACKEND_URL
ENV VITE_BACKEND_WS_URL=$VITE_BACKEND_WS_URL
ENV VITE_APP_COMMIT=$VITE_APP_COMMIT
ENV VITE_APP_BUILD_TIME=$VITE_APP_BUILD_TIME

# The bundle is architecture-independent: it is built once on the build
# platform and copied into every target image.
RUN npm run build


# ---- Stage 2: Build Backend ----
FROM --platform=$BUILDPLATFORM golang:1.26.5-alpine AS backend-builder

# Install build dependencies
RUN apk add --no-cache git

# Set working directory
WORKDIR /app

COPY backend/go.mod backend/go.sum ./
RUN go mod download && go mod verify

# Copy only compilable source. Runtime environment files are never placed in a
# build layer, and an image build must not rewrite go.mod/go.sum.
COPY backend/cmd ./cmd
COPY backend/internal ./internal

# BuildKit fills TARGETOS/TARGETARCH from --platform (or the daemon's own
# platform). Never give them defaults: a default silently overrides the
# automatic value and produces an amd64 binary inside an arm64 image.
ARG TARGETOS
ARG TARGETARCH
RUN test -n "$TARGETOS" && test -n "$TARGETARCH" && \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" go build \
      -buildvcs=false -trimpath -ldflags="-s -w" -o /app/server ./cmd/web

# Compile the real knowledge extraction package into a disposable conformance
# binary. The scratch export target is used only by CI; neither the test binary
# nor the Go toolchain is copied into the production image below.
FROM backend-builder AS knowledge-extraction-test-builder
# ARG values are per stage; re-declare so the child stage sees the platform.
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" go test -c \
      -tags=knowledge_extraction_integration \
      -o /app/knowledge-extraction.test ./internal/handlers

FROM scratch AS knowledge-extraction-test-binary
COPY --from=knowledge-extraction-test-builder \
  /app/knowledge-extraction.test /knowledge-extraction.test


# ---- Stage 3: Final Production Image ----
FROM alpine:3.22

ARG VCS_REF=unknown
LABEL org.opencontainers.image.revision="${VCS_REF}"

RUN apk --no-cache add \
      ca-certificates \
      font-dejavu \
      poppler-utils \
      tesseract-ocr \
      tesseract-ocr-data-eng \
      tesseract-ocr-data-chi_sim \
      tesseract-ocr-data-jpn \
      tesseract-ocr-data-kor \
    && addgroup -S -g 10001 dreamtrans \
    && adduser -S -D -H -u 10001 -G dreamtrans dreamtrans \
    && mkdir -p /app/data \
    && chown -R dreamtrans:dreamtrans /app

WORKDIR /app

COPY --from=backend-builder --chown=dreamtrans:dreamtrans /app/server ./server
COPY --from=frontend-builder --chown=dreamtrans:dreamtrans /app/frontend/dist ./public
# The installer extracts this exact schema bundle from the pulled image ID,
# preventing a mutable Git branch from drifting away from the application.
COPY backend/migrations /usr/share/dreamtrans/migrations
COPY scripts/migrate.sh /usr/share/dreamtrans/migrate.sh
# The backup helper ships with the release too, so the installer can drop it
# next to migrate.sh and keep it current on every --update.
COPY scripts/backup.sh /usr/share/dreamtrans/backup.sh
COPY scripts/release.py /usr/share/dreamtrans/release.py
COPY deploy/release.json /usr/share/dreamtrans/release.json
RUN chmod 0555 /usr/share/dreamtrans/migrations \
      /usr/share/dreamtrans/migrate.sh \
      /usr/share/dreamtrans/backup.sh \
    && chmod 0444 /usr/share/dreamtrans/migrations/*.sql

EXPOSE 8080

ENV RAG_DB_PATH=/app/data/rag.db
ENV RAG_MAX_DB_MB=102400
ENV DREAMTRANS_CONFIG_PATH=/app/data/dreamtrans.config.json
ENV KNOWLEDGE_DATA_PATH=/app/data/knowledge
ENV KNOWLEDGE_EXTRACT_WORKERS=2
ENV KNOWLEDGE_MAX_EXTRACTED_MB=10
ENV KNOWLEDGE_MAX_OFFICE_UNCOMPRESSED_MB=100
ENV KNOWLEDGE_MAX_IMAGE_MEGAPIXELS=40
ENV KNOWLEDGE_MAX_PDF_PAGES=100
VOLUME ["/app/data"]

USER dreamtrans
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --retries=12 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/readyz || exit 1
CMD ["./server"]

COPY scripts/edge-install.py /usr/share/dreamtrans/edge-install.py
COPY scripts/edge-install.sh /usr/share/dreamtrans/edge-install.sh
