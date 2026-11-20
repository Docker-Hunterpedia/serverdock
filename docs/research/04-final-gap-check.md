# Research: Final gap check

*Researched 2026-11-20 by two AI research agents that read features.md, roadmap.md, architecture.md and security.md first. Not yet confirmed by the maintainer. GitHub numbers are reaction counts from the issue/discussion search. Reddit and the HN API were not reachable. Commercial-platform links point to official docs and were not re-fetched.*

## A. What self-host users ask for (GitHub issues/discussions sorted by reactions)

| Gap | Evidence |
|---|---|
| Notice new images / auto-update installed apps and templates | Portainer [#3434](https://github.com/portainer/portainer/issues/3434) (180), #1304 (75); Dokploy #2655 (39); Coolify disc. #3162; Komodo #314, #952, #979 |
| Multiple users before full RBAC | Dokploy [#707](https://github.com/Dokploy/dokploy/issues/707) (58); Coolify disc. #3232, #2378 |
| Per-repo config as code; Terraform/Pulumi provider | Dokploy #1555 (66), #4008 (39); Komodo #431 |
| Adopt existing Compose stacks/containers, import `.env` | Portainer #1857 (85); Komodo #377 (41), #305 |
| Live build / image-pull progress | Komodo #77 (43), #1198 (34); Portainer #919 |
| Restart / change env without rebuild | Coolify disc. #3560, #2854 |
| Per-app CPU/RAM limits, reservations, shm size | Coolify disc. #4661; Dokploy #3975 |
| Preview details: DB per preview, previews from CI images, via API, fork approval | Dokploy #2462, #2386, #1365; Coolify #6571 |
| Config files mounted from UI; Compose with repo-relative files | Portainer #6390 (45); Komodo #92; Coolify #1996 |
| Deploy context variables (FQDN, SHA, PR id, env name) | Coolify disc. #4757; Dokploy #3829 |
| Podman / rootless / non-root agent | Portainer #2991 (80); Coolify disc. #3137; Dokploy #134, #1126; Komodo #684 |
| IPv6 | Coolify #2484 (43) |
| Backup targets beyond S3 (local, SFTP); Postgres PITR | Dokploy #416, #1670; Coolify #7423 |
| Scale-to-zero (sleep idle apps) | Dokploy #2154 |
| Static sites without containers | CapRover #905 |
| External DB access with TLS + IP allowlist | Coolify disc. #3976 |
| Readable container names, per-project alert routing, Azure DevOps/Codeberg, build concurrency | Coolify disc. #3569; Dokploy #572, #1949, #2021, #2000 |

**Operational complaints:** Docker 29 API change broke Portainer (#12925, 327 — the most-reacted issue in the set), Traefik and Watchtower; updates that fail but log success (Coolify #11890) or reset config (#9127); upgrades skipping many versions (Coolify v5 #5685, 693); upsell banners (Portainer #8452, 221) and forced policies (#6904, 124); Compose behaving differently than Docker Compose (Coolify #7655, #5921, #12592); BuildKit cache growing 20+ GB (Coolify #12542); onboarding/docs gaps (Openship #527, Dokploy disc. #490, #1689, #1964); deploys dropping in-flight requests (Coolify disc. #4692).

## B. Commercial PaaS and production operations

| Gap | Reference |
|---|---|
| Deploy strategy for stateful services (stop-first when a writable volume is attached) | Kamal docs |
| Keep logs of previous deploys (blue-green deletes old container logs) | [Heroku logging](https://devcenter.heroku.com/articles/logging) |
| One-off run commands in a fresh container | [Heroku one-off dynos](https://devcenter.heroku.com/articles/one-off-dynos) |
| Release phase semantics (migrations once, before handover; failure aborts; rollback warns on schema) | [Heroku release phase](https://devcenter.heroku.com/articles/release-phase) |
| DB lifecycle: major-version upgrade, PgBouncer, PITR (WAL-G) | [pg_upgrade](https://www.postgresql.org/docs/current/pgupgrade.html), [WAL-G](https://github.com/wal-g/wal-g) |
| TCP/UDP exposure (non-HTTP) | [Railway TCP proxy](https://docs.railway.com/reference/tcp-proxy) |
| Promote between environments, clone environment, required deploy approval | [Heroku pipelines](https://devcenter.heroku.com/articles/pipelines) |
| Preview lifecycle: auto-delete, cap, seed DB, sleep | [Render previews](https://render.com/docs/preview-environments), [Fly autostop](https://fly.io/docs/launch/autostop-autostart/) |
| Rotation: master key, agent certs, token expiry, DB passwords | — |
| i18n + RTL foundation (Arabic) — **rejected by the maintainer: English only** | [CSS logical properties](https://developer.mozilla.org/en-US/docs/Web/CSS/CSS_logical_properties_and_values) |
| Build for the target server's architecture (arm64) | [multi-platform builds](https://docs.docker.com/build/building/multi-platform/) |
| Uninstall / decommission | [Coolify uninstall](https://coolify.io/docs/get-started/uninstallation) |
| Telemetry policy | — |
| Bring-your-own certs / internal CA; air-gapped install | — |
| Support bundle, docs site, onboarding hints | — |
| Usage / cost view per project | Railway, Vercel usage pages |
| Template app update with backup + rollback | [Cloudron updates](https://docs.cloudron.io/updates/) |
| GPU containers with explicit permission | [NVIDIA container toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/) |

**Deliberately excluded:** autoscaling beyond scale-to-zero, CDN caching, Kubernetes, mail, serverless functions.
