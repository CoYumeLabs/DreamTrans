# DreamTrans - A dApp for the Personal Central AI System (PCAS)

[![CI - Code Quality](https://github.com/CoYumeLabs/DreamTrans/actions/workflows/ci.yml/badge.svg)](https://github.com/CoYumeLabs/DreamTrans/actions/workflows/ci.yml)
[![Docker Image CI](https://github.com/CoYumeLabs/DreamTrans/actions/workflows/docker-build.yml/badge.svg)](https://github.com/CoYumeLabs/DreamTrans/actions/workflows/docker-build.yml)

**DreamTrans** is a foundational dApp within the **DreamHub** ecosystem. Its primary role is to provide a powerful, real-time, multilingual transcription and translation service, acting as a core data-ingestion component for the **Personal Central AI System (PCAS)**.

Quick links:

- GitHub repository: https://github.com/CoYumeLabs/DreamTrans
- One‑command deploy: see below
- User Guide: docs/USER_GUIDE.md
- AI context and knowledge guide: docs/RAG.md
- Deployment and migration guide: DEPLOY.md
- Channel invitations and promotional rewards: docs/PROMOTION_INVITES.md
- Signup reward risk controls: docs/SIGNUP_RISK.md
- Environment variables: docs/ENVIRONMENT_VARIABLES.md
- Performance Monitoring: docs/PERFORMANCE_MONITORING.md

## 📱 Unified UI

DreamTrans now uses one responsive React workspace on both `/` and `/pro`.
There is no separate Classic/Pro transcription interface: transcription,
translation, history, export, and the mobile layout all come from the same UI.
`/pro` requires login and enables the authenticated cloud workflow; `/` can use
anonymous local mode only when the server explicitly permits it. The admin
dashboard remains independent at `/pro/admin`.

---

This project serves two purposes:
1.  A fully functional, standalone web application for real-time transcription and translation.
2.  A reference implementation of a "headless" service dApp, demonstrating how to integrate with and provide capabilities to the PCAS event bus.

> **Core Architectural Philosophy**: This project is designed based on the "Personal Data Internet" model. Each dApp (like DreamTrans) is an Autonomous System (AS) that provides specific capabilities. PCAS acts as the core BGP backbone, routing events (data packets) between dApps based on a declarative policy. For more details, refer to [ADR-001: The "Personal Data Internet" Model](./docs/adr-001-personal-data-internet-model.md).

## Current Features (Standalone Web App)

- **Real-Time Transcription & Translation**: Speaker-separated transcription
  powered by Speechmatics, with original, bilingual, and translation-only
  reading modes. Translation defaults to the context-aware AI engine
  (`/ws/translate`, rolling conversation context, sentence-level batching,
  customizable prompt); Speechmatics' built-in per-fragment machine
  translation remains available as a low-latency fallback engine.
- **Long-Session Workspace**: Transcript data is normalized and appended
  incrementally, while the feed renders only the visible window. Long recordings
  do not require rebuilding all prior text or audio on every update.
- **Chunked Local Persistence**: Confirmed transcripts, translations, metadata,
  and optional 96 kbps MP3 frame chunks are stored in IndexedDB. Encoding runs
  in a Worker, so Continue can append another capture without concatenating
  incompatible WebM/MP4 containers. The history list reads metadata only;
  old-format sessions are read only after an explicit migration.
- **Session Lifecycle**: Start a new session, pause/resume a live recording, load
  history, or use **Continue** to append to the same loaded session and timeline.
- **Complete Export**: Export original, translated, or bilingual text. When
  local audio saving is enabled, the complete recording remains downloadable:
  Chromium streams IndexedDB chunks to the selected file, while browsers without
  the file picker assemble a Blob only for that explicit download action.
- **Responsive Desktop and Mobile UI**: The same feature set adapts from a
  desktop sidebar/workspace to touch-friendly mobile sheets and controls.
- **AI Context and Knowledge Projects**: Chat, context preview, summaries,
  notes, and action items share one total input budget. Authenticated projects
  support files and editable memories, PostgreSQL hybrid lexical/semantic
  retrieval, and explicit cost confirmation before semantic indexing.
- **Session Insights**: Session totals, translation progress, local word and
  bi-gram vocabulary tools, CSV export, AI explain shortcuts, and authorized
  server API usage/recent-call views.
- **Resilient Capture and Sync**: The transcription connection reconnects after
  transient interruptions. Authenticated cloud transcript writes first enter a
  durable, account-scoped browser outbox and are retried after reconnect/reload.
- **Balance-Aware Pause**: Insufficient transcription or AI translation funds
  pause audio capture, the timer, and live translation without repeated
  reconnects. Existing content and queued, unsent audio are retained. After
  topping up, explicitly resume the same session; the server checks access
  before reconnecting. No audio is recorded during this pause, and text around
  the original interruption may be incomplete. Stopping remains available
  without paying for another connection.

### Authenticated / Pro Deployment

The authenticated deployment adds these capabilities to the same responsive
workspace:

- **Multi-Tenant Architecture**: PostgreSQL-backed user accounts, tenants, and sessions
- **JWT Authentication**: Secure token-based authentication with refresh tokens
- **Cloud Session Storage**: Save transcripts and translations to the cloud;
  locally retained audio is not uploaded by this workflow
- **Account-Isolated Browser Cache**: Local metadata, transcript records, audio,
  and pending cloud writes are visible only to their owning account. Anonymous
  history remains separate from authenticated history.
- **Offline-Safe Transcript Outbox**: Finalized cloud transcript writes survive a
  refresh and retry when the same user and network return. If the cloud API is
  unavailable, a previously cached cloud session can still be opened for
  reading/export from its account-scoped local copy; the first load of an
  uncached session still requires a network connection.
- **Admin Dashboard**: User management, tenant quotas, usage statistics
- **API Traffic Control**: All Speechmatics and OpenAI API calls routed through backend
  - Admin setting to enable/disable user-provided API keys
  - Usage tracking per user/tenant
  - Server-managed API credentials (default: users cannot bypass server APIs)
- **Unified Responsive UI**: The same long-session-optimized workspace on desktop and mobile

**Environment Variables for Pro Features:**
```bash
# PostgreSQL (Compose derives DATABASE_URL internally)
POSTGRES_DB=dreamtrans
POSTGRES_USER=dreamtrans
POSTGRES_PASSWORD=a-url-safe-random-password

# JWT secrets
JWT_SECRET=your-jwt-secret
JWT_REFRESH_SECRET=your-refresh-secret

# Bootstrap administrator (needed for a fresh Pro DB unless registration is enabled)
ADMIN_EMAIL=you@example.com
ADMIN_PASSWORD=a-unique-strong-password

# Self-registration is opt-in
REGISTRATION_ENABLED=false
REGISTRATION_INVITE_CODE=
# Sign-ups must click an emailed link before they can log in or receive trial
# credit, so REGISTRATION_ENABLED=true needs one mail transport
# (see docs/ENVIRONMENT_VARIABLES.md).
# Either Resend (HTTP API) ...
RESEND_API_KEY=
MAIL_FROM="DreamTrans <no-reply@yourdomain.com>"
# ... or an SMTP relay
SMTP_HOST=
SMTP_PORT=587
SMTP_USERNAME=
SMTP_PASSWORD=
# Optional abuse controls: per-IP hourly sign-up budget, domain allow/block lists
REGISTRATION_RATE_LIMIT_PER_HOUR=5
REGISTRATION_ALLOWED_EMAIL_DOMAINS=
REGISTRATION_BLOCKED_EMAIL_DOMAINS=

# System settings
ALLOW_USER_API_KEY=false  # Set to 'true' to allow users to use their own API keys
```

### Productivity & Controls

- **Continue (same session)**: After loading a completed session, Continue
  resumes its `session_id`, transcript timeline, and audio chunk sequence instead
  of clearing the workspace.
- **Cost-Safe AI Defaults**: Chat and generated artifacts remain explicit user
  actions. Uploading or migrating knowledge never starts a paid semantic
  backfill; the UI previews model, chunks, estimated tokens, and DP before the
  user confirms an index job. Free lexical retrieval remains available.
- **Practical Settings**: Configure source/target language, live translation,
  translation engine (AI context translation or Speechmatics MT), a custom
  translation prompt, follow-scroll, reduced effects, local audio retention,
  AI prompt, and—only when allowed by the administrator—a custom API
  key/base/model. The key is
  tab-scoped and cleared on logout; non-secret base/model preferences can
  persist in this browser.

### Reliability & Observability

- **Provider Retries**: Backend OpenAI-compatible translate, summarize, and chat
  calls retry selected transient upstream/proxy failures with bounded backoff.
- **Bounded AI Requests**: Client-side timeouts prevent ingest and chat requests
  from hanging indefinitely.
- **Server API Metrics**: Authorized users can inspect request/token totals,
  per-feature breakdowns, and recent call model/latency data. This server-side
  view does not claim browser ASR or Speechmatics translation percentile
  measurements.

## The PCAS Ecosystem Vision

DreamTrans is the first step towards a larger ecosystem of interconnected dApps.

- **DreamTrans (This App)**: The **Data Collector**. Its job is to capture the raw, real-time stream of human conversation and convert it into structured, multilingual text data. In the PCAS model, it acts as a "headless" service, providing the `dapp.dreamtrans.translate.stream.v1` capability to the entire ecosystem.
- **DreamNote (Future dApp)**: The **Knowledge Processor**. It will consume data from dApps like DreamTrans, and by leveraging PCAS and Large Language Models (LLMs), it will provide AI-powered summarization, note-taking, and knowledge graph integration.
- **PCAS (The Backbone)**: The central "BGP router" that understands the capabilities of all installed dApps (via their `dapp.yaml` manifests) and routes events between them based on user-defined policies. It transforms simple events into rich, context-aware actions.

### Secure PCAS Workers and Providers

The event worker connects to `PCAS_ADDR` (`127.0.0.1:50051` by default).
Loopback uses plaintext for local development; a non-loopback address uses TLS
by default. Configure `PCAS_CA_CERT` for a private CA,
`PCAS_TLS_SERVER_NAME` when certificate discovery and DNS names differ, and
`PCAS_API_KEY` for Bearer authentication. `PCAS_INSECURE=true` is an explicit
plaintext opt-in, and the worker refuses to send its API key over remote
plaintext. Remote `audio_url` inputs must use HTTPS unless
`AUDIO_URL_ALLOW_HTTP=true`; internal, loopback, link-local, metadata, and
other special IP ranges remain blocked either way.

The streaming provider listens only on `127.0.0.1:50052` by default. A
non-loopback `GRPC_BIND_ADDR` requires a `PCAS_API_KEY` of at least 16
characters and a `PCAS_TLS_CERT`/`PCAS_TLS_KEY` pair. The
`PCAS_ALLOW_INSECURE_REMOTE=true` escape hatch should be limited to an
otherwise protected network. Concurrency defaults to 32 streams through
`PCAS_MAX_CONCURRENT_STREAMS`; server reflection stays off unless
`PCAS_ENABLE_REFLECTION=true`.

Example remote event-worker connection:

```bash
docker run --rm \
  -e SM_API_KEY="..." \
  -e PCAS_ADDR="pcas.example.com:50051" \
  -e PCAS_API_KEY="a-long-independent-service-key" \
  -e PCAS_CA_CERT="/run/secrets/pcas-ca.crt" \
  -v "$PWD/pcas-ca.crt:/run/secrets/pcas-ca.crt:ro" \
  dreamtrans-event
```

Example TLS provider:

```bash
docker build -f backend/Dockerfile.pcas \
  --build-arg MODE=provider -t dreamtrans-pcas .
docker run --rm -p 50052:50052 \
  -e SM_API_KEY="..." \
  -e GRPC_BIND_ADDR="0.0.0.0" \
  -e PCAS_API_KEY="a-long-independent-service-key" \
  -e PCAS_TLS_CERT="/run/secrets/tls.crt" \
  -e PCAS_TLS_KEY="/run/secrets/tls.key" \
  -v "$PWD/tls.crt:/run/secrets/tls.crt:ro" \
  -v "$PWD/tls.key:/run/secrets/tls.key:ro" \
  dreamtrans-pcas
```

## Getting Started & Deployment

This project is fully containerized and designed for easy deployment.

### 🚀 One-Click Installation (Recommended)

The easiest way to get started - just run this command:

```bash
curl -fsSL https://raw.githubusercontent.com/CoYumeLabs/DreamTrans/main/scripts/install.sh | bash
```

The installer will:

- ✅ Check Docker prerequisites
- ✅ Prompt for your API keys
- ✅ Prompt for an administrator email and generate a unique password if requested
- ✅ Set up PostgreSQL automatically (Pro mode)
- ✅ Generate strong database and JWT secrets in a permission-restricted `.env`
- ✅ Start DreamTrans

**Installation Options:**

```bash
# Basic installation (interactive, default port: 16002)
curl -fsSL https://raw.githubusercontent.com/CoYumeLabs/DreamTrans/main/scripts/install.sh | bash

# Custom port
curl -fsSL ... | bash -s -- --port 8080

# Update existing installation
curl -fsSL ... | bash -s -- --update

# Management commands
curl -fsSL ... | bash -s -- --stop      # Stop services
curl -fsSL ... | bash -s -- --start     # Start services
curl -fsSL ... | bash -s -- --restart   # Restart services
curl -fsSL ... | bash -s -- --status    # Show status
curl -fsSL ... | bash -s -- --logs      # Show logs (follow mode)

# Uninstall
curl -fsSL ... | bash -s -- --uninstall
```

### Prerequisites

- Docker & Docker Compose
- An API key from [Speechmatics](https://www.speechmatics.com/) (required)
- OpenAI API key (optional, for the AI assistant and RAG ingestion)

### Manual Installation with Docker Compose

1. **Clone the repository:**
   ```bash
   git clone https://github.com/CoYumeLabs/DreamTrans.git
   cd DreamTrans
   ```

2. **Create environment file:**
   ```bash
   cp backend/.env.example .env
   # Edit .env and set every required API key, database password,
   # JWT secret, and (for a fresh Pro database) a bootstrap administrator pair.
   ```

   There are no production password fallbacks or preinstalled administrator
   credentials. Generate independent secrets, for example with
   `openssl rand -hex 32`.

3. **Start services:**
   ```bash
   docker compose up -d
   ```

   Compose runs the checksummed migration job before starting DreamTrans. This
   also applies missing migrations when `postgres_data` already exists. Keep
   the checkout release and `IMAGE_TAG` aligned.

4. **Access the application:**
   - Unified workspace: http://localhost:16002
   - Authenticated entry: http://localhost:16002/pro

### Production Deployment (Simple Docker Run)

For a local-only unified workspace without PostgreSQL, bind to loopback and explicitly
enable anonymous compatibility mode:

```bash
docker run -d \
  --name dreamtrans \
  -p 127.0.0.1:16002:8080 \
  -e SM_API_KEY="your_speechmatics_api_key" \
  -e OPENAI_API_KEY="your_openai_api_key" \
  -e ALLOW_ANONYMOUS_API=true \
  -v dreamtrans_data:/app/data \
  --restart unless-stopped \
  ghcr.io/coyumelabs/dreamtrans:latest
```

Do not publish that anonymous mode on `0.0.0.0`. For a network-accessible
headless/API deployment, leave `ALLOW_ANONYMOUS_API=false`, generate
`DREAMTRANS_API_KEY` (and a separate `DREAMTRANS_ADMIN_API_KEY`), then send the
service key in `X-DreamTrans-API-Key`; WebSocket clients may use the `api_key`
query parameter. Without a JWT, service key, or explicit anonymous mode,
provider-backed endpoints correctly return `401`.

For browser access over a network, the PostgreSQL-backed Compose deployment is
recommended: log in through Pro and use JWT authentication. Self-registration
is disabled by default; enable `REGISTRATION_ENABLED=true` only deliberately,
preferably with `REGISTRATION_INVITE_CODE`.

## Documentation

Please see the docs folder for complete guides:

- docs/USER_GUIDE.md — UI overview, global settings, quick start
- docs/RAG.md — RAG pipeline and APIs
- docs/COURSE_TIMETABLE.md — weekly class times and automatic session filing
- docs/PERFORMANCE_MONITORING.md — Tokens/Latency/Model metrics
- docs/ENVIRONMENT_VARIABLES.md — environment configuration
- docs/DOCKER_DYNAMIC_CONFIG.md — deployment options

## Defaults & Endpoints (Quick Reference)

- Default models
  - OpenAI-compatible translate endpoint: `gpt-5.6-luna`
  - Chat: `gpt-5.6-sol`
  - Summary: `gpt-5.6-sol`
- Core Endpoints
  - `/healthz` — process liveness (`GET`/`HEAD`, no upstream calls)
  - `/readyz` — readiness, including a bounded PostgreSQL ping when configured
  - `/api/models/defaults` — backend default model set (Chat/Translate/Summary)
  - `/api/prompts/defaults` — default system prompts
  - `/api/metrics` + `/api/metrics/reset` — Super Admin process-level usage
    snapshot and reset
  - `/api/rag/ask` — RAG Q&A (supports per‑request overrides)
  - `/api/rag/summary` — current session summary
  - `/api/rag/title` — cached session title (generated once, then reused)
- Pro Endpoints (requires PostgreSQL)
  - `/api/auth/register` — user registration
  - `/api/auth/login` — user login (returns JWT)
  - `/api/auth/refresh` — refresh access token
  - `/api/user/profile` — get/update user profile
  - `/api/sessions` — list/create cloud sessions
  - `/api/sessions/{id}` — get/update/delete session
  - `/api/sessions/{id}/transcripts` — save transcripts
  - `/api/admin/users` — admin: list/manage users
  - `/api/admin/tenants` — admin: list/manage tenants
  - `/api/admin/settings` — admin: update system settings
  - `/api/system/settings` — public: get system settings (allow_user_api_key)
  - `/ws/speechmatics` — authenticated Speechmatics WebSocket proxy; anonymous
    access is possible only when the server explicitly enables anonymous APIs

## License

This project is licensed under the **PolyForm Noncommercial License 1.0.0**.

### What this means:

✅ **Allowed:**
- Personal use, research, and experimentation
- Educational and academic use
- Use by non-profit organizations
- Creating derivative works (non-commercial only)
- Distributing copies with this license

❌ **Prohibited:**
- **Any commercial use whatsoever**
- Selling or monetizing this software
- Using in commercial products or services
- Providing commercial SaaS services with this software

### Commercial Licensing

If you need to use DreamTrans for commercial purposes, please contact the author for a commercial license.

**Unauthorized commercial use is copyright infringement and may result in legal action.**

See the full [LICENSE](./LICENSE) file for details.
