# syntax=docker/dockerfile:1
#
# claude-local-api — batteries-included container image.
#
# This service does NOT talk to Claude directly: it shells out to the Claude
# Code CLI in headless mode. And the CLI does not natively render images/video
# — it GENERATES files by writing and running code. So the runtime image bundles:
#
#   1. the compiled Go server,
#   2. the Claude Code CLI (a Node.js program), and
#   3. a media/data toolchain (Python+matplotlib/Pillow, ffmpeg, ImageMagick,
#      ghostscript, pandoc, poppler, zip) so the agent can actually produce
#      charts, images, video, PDFs, and archives on request.
#
# The CLI's subscription (OAuth) login lives under $HOME and is NOT baked into
# the image — mount it at runtime (see docker-compose.yml and the README).

############################
# Stage 1 — build the Go server binary
############################
FROM golang:1.24-bookworm AS builder

WORKDIR /app

# Download modules first so this layer is cached until go.mod changes.
COPY go.mod ./
RUN go mod download

# Compile a small, static, stripped binary.
COPY src ./src
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
        -o /out/claude-local-api ./src/cmd/claude-local-api

############################
# Stage 2 — runtime: Go binary + Claude Code CLI + media/data toolchain
############################
FROM node:22-bookworm-slim AS runtime

# System packages (ordered first so this expensive layer caches well):
#   ca-certificates, git  — TLS + common agent git operations
#   python3 + venv        — scripting; numpy/pandas/matplotlib/Pillow/reportlab
#   ffmpeg                — audio/video generation & transcoding
#   imagemagick           — image conversion (`convert`)
#   ghostscript, poppler  — PDF rasterize/inspect (prefer over IM's PDF coder)
#   pandoc                — markdown/HTML -> DOCX/HTML (no PDF engine bundled;
#                           generate PDFs with reportlab or ghostscript instead)
#   zip/unzip/xz-utils    — archive create/extract
#   fonts-*               — so matplotlib/pandoc/ImageMagick can render text
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates git \
        python3 python3-venv \
        ffmpeg imagemagick ghostscript poppler-utils pandoc \
        zip unzip xz-utils \
        fonts-dejavu fonts-liberation fonts-noto-core \
    && rm -rf /var/lib/apt/lists/*

# Python libraries in an isolated venv (Debian's system Python is "externally
# managed" / PEP 668, so we never pip-install into it). /opt/venv/bin on PATH
# makes `python3`/`pip` resolve to the venv with no activation step.
ENV PATH=/opt/venv/bin:$PATH
COPY requirements.txt /tmp/requirements.txt
RUN python3 -m venv /opt/venv \
    && /opt/venv/bin/pip install --no-cache-dir --upgrade pip \
    && /opt/venv/bin/pip install --no-cache-dir -r /tmp/requirements.txt

# The Claude Code CLI (Node.js). Global install needs root.
RUN npm install -g @anthropic-ai/claude-code \
    && npm cache clean --force

# Also put the venv on PATH for login shells (ENV PATH covers non-login
# shells; this covers any tool that runs commands via a login shell), so the
# agent's `python3` always resolves to the venv with the bundled libraries.
RUN printf 'export PATH=/opt/venv/bin:$PATH\n' > /etc/profile.d/venv.sh

# The compiled server.
COPY --from=builder /out/claude-local-api /usr/local/bin/claude-local-api

# Run as an unprivileged user. The Claude login is stored under this user's home
# directory; ephemeral run dirs live in /runs (a container-internal volume).
RUN useradd --create-home --uid 10001 appuser \
    && mkdir -p /workspace /runs \
    && chown -R appuser:appuser /workspace /runs /home/appuser
USER appuser
ENV HOME=/home/appuser

# /workspace is the user's mounted repo (legacy in-place path); /runs holds the
# auto-managed per-request workspaces for file uploads/outputs.
WORKDIR /workspace
ENV CLAUDE_API_ADDR=:8787 \
    CLAUDE_DEFAULT_CWD=/workspace \
    CLAUDE_WORKSPACE_ROOT=/runs \
    CLAUDE_FORCE_SUBSCRIPTION=true \
    CLAUDE_JSON_LOGS=true

EXPOSE 8787

# Liveness probe against the built-in health endpoint (node ships global fetch).
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD node -e "fetch('http://127.0.0.1:8787/healthz').then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"

CMD ["claude-local-api"]
