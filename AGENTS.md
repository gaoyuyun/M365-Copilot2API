# Repository Instructions

## Upstream Synchronization

- Rebase upstream changes with `./scripts/sync-upstream.sh`; do not run a plain upstream rebase.
- Keep `.github/workflows/publish-container.yml` as the only GitHub Actions workflow.
- Never restore or copy GitHub Actions workflows from upstream.
