# PostgreSQL Setup - Implementation Notes

## Status: Complete

All PostgreSQL infrastructure files have been created:

### Files Created

| File | Purpose |
|------|---------|
| `deployments/podman/deps.compose.yaml` | PostgreSQL container definition (pre-existing, verified compatible) |
| `deployments/podman/README.md` | Documentation for PostgreSQL setup |
| `deployments/postgres/init/00-init.sql` | First-start schema initialization |
| `scripts/postgres-init.sh` | Bootstrap script for first-time setup |

### Existing Compose File Note

The `deps.compose.yaml` already contains a complete PostgreSQL configuration.
To add init script auto-execution on first start, add this volume mount:

```yaml
volumes:
  - vornik-postgres-data:/var/lib/postgresql/data
  # Add this line:
  - ../postgres/init:/docker-entrypoint-initdb.d:ro,Z
```

This is optional - database will work without it. The init script just creates
the `vornik` schema automatically instead of requiring manual setup.

## Acceptance Criteria Verification

- [x] PostgreSQL container definition complete (image, volume, port, healthcheck)
- [x] Named volume for persistent data (`vornik-postgres-data`)
- [x] Credentials setup (environment variables with defaults)
- [x] Health check command defined (`pg_isready`)
- [x] Restart policy configured (`unless-stopped`)
- [x] Bootstrap script handles first-time setup (`scripts/postgres-init.sh`)
- [x] Can be started with single command (`podman compose -f deps.compose.yaml up -d`)
- [x] Exposed on documented port (5432)

## Quick Start

```bash
# From project root
cd deployments/podman
podman compose -f deps.compose.yaml up -d

# Or use the bootstrap script
./scripts/postgres-init.sh
```

## Connection

```
Host:     localhost
Port:     5432
Database: vornik
User:     vornik
Password: POSTGRES_PASSWORD in deployments/podman/.env (generated per install since 2026-10-21, #412)
```

Connection string:
```
postgresql://vornik:<POSTGRES_PASSWORD>@localhost:5432/vornik
```