# GitHub Actions CI/CD

DreamTrans and YuAction have independent quality and publication gates. Component and native AMD64/ARM64 image builds run in parallel. Publication loads and pushes the exact validated image artifacts without rebuilding.

- `ci.yml`: DreamTrans and Edge tests, migrations, lifecycle checks and image verification.
- `yuaction.yml`: YuAction tests, browser suite, migrations and live upgrade verification; triggered by YuAction or shared dependencies.
- `docker-build.yml`: reusable artifact publication, called only after the product's complete quality gate succeeds.
- Pull requests verify images without pushing. Main and version releases publish only the corresponding product's validated artifacts.

See [the complete CI/CD guide](CI_README.md) for triggers, cache/runner layout, image tags, release pairing and local checks, and [the monorepo deployment guide](../../docs/deployment/yuaction-monorepo.md) for existing installations.

The workflow uses the repository's `GITHUB_TOKEN`; only publication jobs receive package write permission. Public installations require the GHCR packages to be public, or a configured registry login.
