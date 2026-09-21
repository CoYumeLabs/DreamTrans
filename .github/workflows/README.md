# GitHub Actions CI/CD

> 主站与 YuAction 统一由 `ci.yml` 验证，`docker-build.yml` 在全部检查通过后发布四个组件镜像。YuAction 保持独立端口；当前流程见 [合仓说明](../../docs/deployment/yuaction-monorepo.md)。


This workflow automatically builds and pushes Docker images to GitHub Container Registry (ghcr.io).

## Workflow Triggers

- **Push to main branch**: Builds and pushes only the images whose Docker inputs
  changed; the application image receives the `latest` tag
- **Push tags**: Builds and pushes with version tags (e.g., `v1.0.0`)
- **Pull requests**: Builds but doesn't push (for testing)
- **Manual trigger**: Can be run manually from Actions tab

## Image Tags

The workflow creates multiple tags:
- `latest` - Always points to the latest main branch build
- `main` - Tracks the main branch
- `v1.0.0` - Semantic version tags (when you push a tag)
- `1.0` - Major.minor version tags
- `main-abc1234` - Branch name with short commit SHA
- `sha-abc1234` - Full SHA reference

## Multi-platform Support

Images are built for both:
- `linux/amd64` (Intel/AMD processors)
- `linux/arm64` (ARM processors, e.g., Apple Silicon, AWS Graviton)

Every Dockerfile pins its builder stages to `--platform=$BUILDPLATFORM` and
cross-compiles Go with `GOARCH=$TARGETARCH`; the frontend bundle is built once
and shared. QEMU is only used for the small runtime stage (`apk add`), so the
arm64 image no longer runs npm, Vite or the Go compiler under emulation. Do not
give `ARG TARGETOS` / `ARG TARGETARCH` default values: a default overrides the
value BuildKit injects from `--platform` and yields an amd64 binary inside the
arm64 image.

## Usage

### 1. First Time Setup

The workflow uses `GITHUB_TOKEN` which is automatically provided. No additional secrets needed.

### 2. Make the Image Public (Optional)

By default, packages are private. To make your image public:
1. Go to your GitHub profile → Packages
2. Find the `dreamtrans` package
3. Click Package settings → Change visibility → Public

### 3. Pull and Run

```bash
# Pull the latest image
docker pull ghcr.io/coyumelabs/dreamtrans:latest

# Run with environment variables
docker run -d \
  --name dreamtrans \
  -p 8080:8080 \
  -e SM_API_KEY=your_speechmatics_api_key \
  ghcr.io/coyumelabs/dreamtrans:latest
```

### 4. Using Specific Versions

```bash
# Use a specific version
docker pull ghcr.io/coyumelabs/dreamtrans:v1.0.0

# Use a specific commit
docker pull ghcr.io/coyumelabs/dreamtrans:main-abc1234
```

## Monitoring Builds

1. Go to Actions tab in your repository
2. Click on "Build and Push Docker Image" workflow
3. View build logs and deployment instructions

## Caching

The workflow uses GitHub Actions cache to speed up builds:
- Docker layer caching
- Independent cache scopes for the application, event worker, and provider
  images, so concurrent builds cannot overwrite one another
- Multi-stage build optimization
- Dependency caching

Newer runs for the same workflow and Git ref cancel obsolete in-progress runs.
Code-quality checks and production-image verification also run in parallel.

## Troubleshooting

If builds fail:
1. Check Actions tab for error logs
2. Ensure Dockerfile is valid
3. Verify all source files are committed
4. Check for sufficient GitHub Actions minutes
