# Plan 0005 — Phase 1 M3b: service configuration

**Status:** APPROVED (2026-11-28), Revision 1 plus round-2 fixes. `reviewer`, `security` (required), `systems` (required, including the network-join decision, [Revision 1](#revision-1-review-fixes) item B1) and `footprint` approve. The maintainer accepted every recommended answer and the R3 trade-off ([answers](#questions-for-the-maintainer)). Entry gate still open: PR #6 (M3a) merged and plan 0002 Revision 1 (PR #7, with the S4 re-base) merged before step 1.

**Author:** planner (AI) · **Date:** 2026-11-28

**Roadmap:** Phase 1, milestone M3b of [plan 0002](0002-phase-1-mvp-solo.md). It is run order 4 and part of the **early usable version** together with M3a, M4a and M7a (plan 0002 Revision 1). It serves these roadmap items (plan 0002 Part C numbering):
- **8** "env vars & secrets (encrypted, audited reveal)": the M3b part. Secretbox exists since M2.
- **12** "private registry credentials; persistent volumes; per-project network with service DNS": all of it.
- **14** "per-app CPU/RAM limits": the M3b part. Stop-first and draining are M3a.
- **15** "deploy context variables": the M3b part.
- **17** "Docker RC in CI; Docker version-change warning": the M3b part.
- **23** "accessible, phone-usable UI": the new screens.

This plan is AI-written and not confirmed by the maintainer.

**Binding inputs:**
- **Plan 0002:**
  - phase-wide rules 1–11;
  - the M3b section, listed in [Traceability](#traceability);
  - the footprint allocation and its re-base rule;
  - Revision 1: early use is for apps with disposable or externally backed-up data until M6;
  - "M3b adds known secret-store values" to redaction.
- **Plan 0004:** the notes for plan 0005, and the M3b follow-ups in Revisions 8, 10, 17 and 21, listed in [Traceability](#plan-0004-hand-offs--where).
- **[`docs/security.md`](../security.md) Draft 4:**
  - the A4 rows "Planned (M3b)";
  - the accepted risks: no per-container limits until M3b, N13, deploy logs holding app credentials, and registry hosts checked only cosmetically;
  - the A6 row "Secrets in logs".
- **[Research 06](../research/06-docker-pre28-unpublished-ports.md):** the Docker < 28 warning and the `nat-unprotected` refusal.
- **ADRs:**
  - 0004 (with addendum A), 0008, 0009 and 0010;
  - [0011](../decisions/0011-footprint-ceiling.md): tripwires S2 80 MB and S4 50 MB;
  - [0012](../decisions/0012-docker-client-and-op-protocol.md).
- **The M3a code at `5eded76`:**
  - `internal/secretbox`;
  - `internal/policy`: `CheckCreate`, refs, addresses;
  - `internal/deploy`: snapshot v1, `BuildCreate`, the runner, `restoreOld`, the boot pass `interrupted.go`, the reconciler, deploy logs;
  - `internal/agentops`;
  - `internal/agent`: `MaxRequestLine` is 64 KiB, and request lines are encoded with `json.Marshal`;
  - `internal/agent/dockerapi`: `NetworkEnsure`, `Client.Info`;
  - `internal/actions`: `system.docker` calls `dockerapi.Client.Info` directly;
  - `internal/redact`: `redact.New`;
  - migrations 0001–0004.
- **Maintainer decisions** of 2026-11-27.
- **The M3a CI bench on PR #6** (2026-11-28; median of per-run medians), the M3b baseline:
  - **S2 live watcher 54.0 MB;**
  - **S4 46.5 MB;**
  - idle CPU 0.00 %;
  - binary 20.0 MB gzip.

## Revision 1 (review fixes)

Each round-1 item and where it landed:

| # | Finding | Where it landed |
|---|---|---|
| **B1** | A candidate that joins the project network only after its health gate cannot reach `db` by name while it is checked. Apps that connect at start would fail every deploy. | **Decision: the candidate joins the project network at create time, with its service alias.** The reason is in [Per-project network](#per-project-network-and-service-dns). The `attach` stage and the `network_connect` op are dropped. `CheckCreate` allows two endpoints. Goal 5 is reworded. `TestCandidateReachesPeersByNameDuringItsHealthGate` is added in step 9. |
| 1 | SQLite cannot `ADD COLUMN … UNIQUE` | `projects.subnet` is a plain column. The partial unique index `projects_subnet` covers rows `WHERE subnet IS NOT NULL`. |
| 2 | Mounts, limits and no-new-privileges were read at run time | These are now frozen in snapshot v2 at enqueue, for deploy, redeploy and rollback. The effective strategy and `timeout_s` are computed from the recorded mounts. The runner builds only from the snapshot. `pending_changes` compares against the active snapshot. Tests are in step 7. |
| 3 | Attach failure under stop-first; boot-pass rule; kill -9 at attach | There is no attach stage any more. A project-network failure is a create failure: blue-green removes the candidate; stop-first goes through `restoreOld` and ends `app_safe` yes or `previous_version_down`. There is no new stage, so `interrupted.go` gets no new rule. The old container keeps its project endpoint (tested). Step 9 adds blue-green and stop-first variants and kill -9 at `start` with two networks. |
| 4 | `env_set` pointer rules | No new version when the set equals the current one. When it equals the live version, the service points back at the live version. `applied` means "current is live". A mismatched `base_version` returns 409. |
| 5 | Project network pool and races | Each project gets the first free /25 of `10.250.0.0/16`, in one `WriteTx` with `UPDATE … WHERE subnet IS NULL`. The plan says what happens when nothing is free. `meta.project_pool` and the /18 pool are dropped; `--project-pool` stays only as an escape hatch. The network is removed only after `deleteRows` actually deleted the project row (decided in the same transaction, removed after it commits). Ensure re-runs once on not-found. A 409 means inspect and compare. A new resync rule, `reconciler.remove_network`. An overlap refuses only that project. A race test covers delete versus a new service. |
| 6 | `delete_volumes` | Order: containers → volumes → rows. `delete_volumes` travels in the payload, and Delete again asks again. A volume in use is reported "kept: in use". Detached rows are kept with `detached_at`. `volume_remove` checks the service id. `volume_ensure` refuses any driver other than `local`. `CheckCreateAgent` ties the volume's `s<id>` to the container's service label. |
| 7 | OOM | `services.last_oom_at`, written from the `oom` event at most once a minute per service. The claim that inspect can recover a missed OOM is dropped (step 2 confirms `OOMKilled` is cleared on restart). The plan states that an OOM during an events gap can be missed. The "was restarted" wording is fixed. |
| 8 | `admin reset` with a wrong key | Refused with app ciphertexts and a missing **or wrong** key unless `--forget-app-secrets`. The `LoadOrCreate` messages are reworded. The `env_lost` flag. `ON DELETE RESTRICT` on `env_version_id`. Env is loaded `WHERE id = ? AND service_id = ?`. |
| 9 | There is no `info` op | The control plane's direct `dockerapi.Client.Info` is the one source of host facts. `system.docker` is marked changed. The version is parsed leniently. A failed `Info` never fails a resync. Recovery steps for a `nat-unprotected` M3a network. |
| 10 | Redactor | Built on `redact.New`, streaming with hold-back. It also covers line pieces of multi-line secrets and the cleaned, JSON-escaped and `%q` forms. Redaction runs before truncation. It fails closed. A failed `AuditRead` intent returns no log. Each case has a test. The sentinel wording is fixed. |
| 11 | Env cap definition | The cap is the exact byte length of the `json.Marshal` request encoding (HTML escaping on). It is enforced at `env_set` and `services.create`, and the agent measures the same thing. A worst-case test checks against `agent.MaxRequestLine`. Memory per deploy is stated. |
| 12 | Bind mounts | A denied set of real paths, with ancestors refused. The check applies to resolved sources. Overlapping sources are refused. The resolved path is sent, and the agent re-resolves it. Refused when ServerDock runs in a container. Owner and "other"-bit rules. Printable paths only. `nosuid` and recursive read-only are documented. The accepted risk is rewritten. A new A5 row. |
| 13 | `keep` and the secret flag | `keep` requires `secret: true`. The flag comes from the decrypted document. |
| 14 | New service with env | `services.create` (changed) takes `env`, with values tagged `sd:"secret"`, in one transaction. A sentinel covers its audit rows. Deploy now falls back to `services.deploy`. |
| 15 | Rollback env and write tokens | A `write` token may roll back only to the current env version; otherwise `admin` is needed, checked in the enqueue transaction. The dialog warns. |
| 16 | Admin can read every secret | New accepted risk. QM3b-11 (fresh 2FA, no tokens, for `set_source`, image changes through `update` and rollback to another repository on services with a bind mount). |
| 17 | Footprint | Full S4 after step 4; QUICK S4 and a goroutine diff in step 4. `footprint` reviews steps 4, 7, 10 and 11, with S2 checks in 10 and 11. An events-filter test. Methods for S10 and S11. `deploys.log` peak. Per-job cost × 2 workers. SPA chunk sizes. RC CI triggers, with only stable required. QM3b-10 becomes a note. |
| NITs | — | AAD `service_env_versions/<service_id>/v<version>`, so there is no insert-then-seal; registries AAD by host. CHECK constraints. More registry ranges, and refusal if any resolved address is blocked; loopback needs the flag only. Env values out of agent errors. Container-name DNS. Isolation "with Docker's firewall rules on". Blue-green DNS in architecture.md. Queued M3a jobs keep their `timeout_s`. QM3b-6 amends plan 0002's exit item. Env and registry actions are registered only in step 7. Tagging all values secret is harmless. `docker network rm` is accepted. Reviews list matched to the steps. security.md updates in step 15. Docker Hub `serveraddress` checked in step 2. |

## Entry gate

1. **M3a is merged** (PR #6) with its CI bench numbers recorded.
2. **Plan 0002 Revision 1 is approved by `reviewer`.** That reorder PR also applies its own S4 re-base, so M3b S4 becomes 46.7 MB.
3. **This plan is approved by `reviewer`**, with sign-off from `security` (required) and `systems` (including B1).
4. **The maintainer has answered [QM3b-1 to QM3b-11](#questions-for-the-maintainer).**

## Goal

1. **Environment variables and secrets per service.**
   - Values are encrypted at rest, AAD-bound.
   - Secret values are write-only.
   - `services.env_reveal` is **`Sensitive`**: fresh 2FA, tokens denied, and **the intent row is written first, so nothing is revealed if that write fails**.
   - Each change is an immutable **env version**, and **a deploy snapshot references its env version**.
2. **Known app secrets never leak.** They never appear in:
   - `deploys.log`, or `deploys.get` reasons and stage details;
   - audit rows;
   - server or Caddy logs;
   - job payloads or job errors;
   - the database bytes.

   A sentinel test proves this. Transformed values and secrets the app generates itself can still reach a deploy log; this is stated honestly.
3. **Private registry credentials.**
   - Stored encrypted.
   - Sent only with the pull that needs them, in the `X-Registry-Auth` header of one `image_pull` op.
   - Never stored on the Docker host.
   - Registry hosts get rule-10 address checks, as far as a fetch made by the Docker daemon allows.
4. **Persistent volumes.**
   - Named volumes.
   - **Bind mounts are admin-only.** They are off until the operator names host directories at the console, and their path validation is fuzzed.
   - **A writable volume makes the deploy stop-first.**
5. **A per-project network with service DNS.**
   - Services in one project reach each other by service name (`http://api:8080`), **including a new version during its health check**.
   - With Docker's firewall rules on, services in different projects cannot reach each other directly.
   - During a blue-green deploy the name resolves to both versions, from the new version's start until the old one stops.
   - A removed candidate never stays resolvable.
6. **CPU and memory limits per service. OOM kills are reported.**
7. **Context variables** `SD_FQDN`, `SD_COMMIT`, `SD_ENV` and `SD_DEPLOY_ID`.
8. **Docker engine awareness.**
   - The version is kept in `meta`.
   - A version change shows "Needs attention" until someone acknowledges it.
   - **An engine before 28.0.0 shows a standing warning.**
   - **A `nat-unprotected` network is refused**, with recovery steps.
9. **Integration suite in CI on Docker stable and the latest RC.**
10. **The M3a hand-offs measured:** the cold-pull peak and a many-redeploy leak loop.
11. **Footprint.**
    - S2 ≤ 58 MB.
    - S4 at 46.7 MB, within the run spread.
    - **No new timers or idle goroutines.**
    - No new dependency.

## Out of scope

- **Domains, app HTTPS and IPv6** (M4a). `SD_FQDN` is empty until then.
- **Git builds** (M5). `SD_COMMIT` is empty for image deploys.
- **Environments** (Phase 2). `SD_ENV` is always `production`.
- **Shared env groups, project variables, `${REF}` references, `.env` import, and build-time variables.**
- **Volume backups.** M6 adds database backups; volume backups are Phase 2. Early use stays limited to apps with disposable or externally backed-up data until M6, and the Storage screen says so.
- **A volume browser, resizing, non-`local` drivers, and driver options.**
- **A cleanup UI for unattached volumes and images** (M8).
- **Per-container disk quotas** (QM3b-2). The 1 GiB free-space check before a pull stays; disk alerts are M8.
- **Firewalling containers.** B10 stays as it is.
- **Credential tests, credential helpers, and per-service credentials.**
- **Changing a project's subnet once it is allocated.**
- **Remote-agent deploys.** Every new op is `LocalOnly`.

## Design

### Overview: what changes on the deploy path

```
enqueue (deploy | redeploy | rollback) ─▶ snapshot v2, frozen:
    { image, env_version_id, health, effective strategy, timings, mounts, memory, cpus, nnp }
    effective strategy = stop_first if any recorded mount is writable; timeout_s derived from it
jobs.run_deploy (builds ONLY from the snapshot):
  pull   : registry address check (rule 10) ─▶ image_pull { ref, stall_s, auth? }
  check  : CheckImage; app address; volume_ensure per recorded named volume;
           project subnet (first use) + project_network_ensure;
           env decrypted (WHERE id = ? AND service_id = ?) ─▶ Env = user vars + SD_* vars;
           bind sources resolved; BuildCreate ─▶ CheckCreateControlPlane (exact)
  start  : container_create with TWO endpoints: app network {IPv4Address}, project network {Aliases:[service]}
           (the agent inspects the project network first, as ourNetwork does for the app network, and requires our instance label, serverdock.project = N and Internal: true)
           (agent re-resolves bind sources, CheckCreateAgent on the exact bytes) ─▶ start ─▶ inspect
  health : unchanged; the candidate already resolves peers by name
  switch, commit, drain, stop_old : unchanged
Every reason, stage detail and kept log passes the deploy's secret redactor first (fail closed).
```

Unchanged:
- the commit order and the stages (no stage is added);
- the route lock;
- the reconciler's container rules;
- the op protocol.

### Concepts and names

- **Env version.** An immutable, encrypted set of variables for one service.
  - The service row points at its current version.
  - A change applies at the next deploy ("Saved. Deploy to apply.").
- **What a deploy records.** Snapshot v2 is frozen at enqueue for every kind of deploy. The runner reads nothing else.

  | Field | `services.deploy` | `services.redeploy` | `services.rollback {to}` |
  |---|---|---|---|
  | Image | desired `image_ref` | live deploy's pinned image | target's pinned image (M3a) |
  | Container port, health, strategy setting, drain, stop grace | service now | live deploy's (M3a) | target's (M3a) |
  | **Env version** | **service's current** | **service's current** (how a variable change is applied) | **target's** (QM3b-7) |
  | **Mounts, memory, CPU, no-new-privileges** | service now | service now | service now |
  | **Effective strategy** | from the recorded mounts and strategy setting | same | same |
  | Public port, registry credential, project network | read at run time (service settings, not in the snapshot) | same | same |

  - A rollback never detaches data and never undoes a limit that was raised to fix an OOM.
  - `set_volumes` or `set_resources` after enqueue does not change the queued deploy.
  - `pending_changes` compares the service's env version, mounts, limits and no-new-privileges with the **active snapshot**.
- **Effective strategy.** A writable recorded mount forces stop-first.
  - The snapshot records the effective `strategy` and `strategy_forced: true`.
  - `jobs.timeout_s` follows the effective strategy.
  - Jobs queued under M3a keep their `timeout_s`.
  - The UI says "Stop-first (required while this service has a writable volume)".
- **Registry entry.** One row per host. It holds an optional username, an encrypted secret, and `allow_private`, which is rule 10's per-target allowlist.
- **Project network.**
  - One internal bridge per project, `serverdock-<install id[:8]>-p<project id>`.
  - Labels: `managed`, `instance`, `project`.
  - Subnet: the project's own /25.
  - Containers join at create time with one alias, the service name.
  - Containers stay on the M3a app network too.

### Data model: migration `0005_service_config.sql`

Once step 1 merges, 0005 is never edited. `foreign_keys(1)` holds.

```sql
CREATE TABLE service_env_versions (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  service_id  INTEGER NOT NULL REFERENCES services(id) ON DELETE CASCADE,
  version     INTEGER NOT NULL CHECK (version >= 1),
  ciphertext  BLOB NOT NULL,                    -- secretbox, AAD "service_env_versions/<service_id>/v<version>"
  names_json  TEXT NOT NULL,                    -- names and flags for display only; never authoritative
  actor_kind  TEXT NOT NULL, actor_id TEXT NOT NULL,
  created_at  TEXT NOT NULL,
  UNIQUE(service_id, version)
) STRICT;
CREATE TRIGGER service_env_versions_immutable BEFORE UPDATE ON service_env_versions
  BEGIN SELECT RAISE(ABORT, 'env versions are immutable'); END;

CREATE TABLE service_volumes (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  service_id  INTEGER NOT NULL REFERENCES services(id) ON DELETE CASCADE,
  kind        TEXT NOT NULL CHECK (kind IN ('volume', 'bind')),
  name        TEXT,                             -- volume: short name ("data")
  host_path   TEXT,                             -- bind: validated absolute host path as entered
  target      TEXT NOT NULL,
  read_only   INTEGER NOT NULL DEFAULT 0 CHECK (read_only IN (0, 1)),
  detached_at TEXT,                             -- removed from the list; the Docker volume is kept
  created_at  TEXT NOT NULL,
  CHECK ((kind = 'volume' AND name IS NOT NULL AND host_path IS NULL)
      OR (kind = 'bind' AND name IS NULL AND host_path IS NOT NULL))
) STRICT;
CREATE UNIQUE INDEX service_volumes_target ON service_volumes(service_id, target) WHERE detached_at IS NULL;
CREATE UNIQUE INDEX service_volumes_name ON service_volumes(service_id, name) WHERE name IS NOT NULL;

CREATE TABLE registries (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  host          TEXT NOT NULL UNIQUE,           -- normalised; never edited (a new host is a new row)
  username      TEXT,
  secret        BLOB,                           -- secretbox, AAD "registries/<host>/secret"
  allow_private INTEGER NOT NULL DEFAULT 0 CHECK (allow_private IN (0, 1)),
  created_at    TEXT NOT NULL, updated_at TEXT NOT NULL,
  CHECK ((username IS NULL) = (secret IS NULL))
) STRICT;

ALTER TABLE services ADD COLUMN env_version_id INTEGER REFERENCES service_env_versions(id) ON DELETE RESTRICT;
ALTER TABLE services ADD COLUMN memory_mb INTEGER CHECK (memory_mb IS NULL OR memory_mb >= 32);
ALTER TABLE services ADD COLUMN cpus_milli INTEGER CHECK (cpus_milli IS NULL OR (cpus_milli >= 50 AND cpus_milli % 50 = 0));
ALTER TABLE services ADD COLUMN no_new_privileges INTEGER NOT NULL DEFAULT 0 CHECK (no_new_privileges IN (0, 1));
ALTER TABLE services ADD COLUMN last_oom_at TEXT;
ALTER TABLE projects ADD COLUMN subnet TEXT;
CREATE UNIQUE INDEX projects_subnet ON projects(subnet) WHERE subnet IS NOT NULL;
```

- **AAD without row ids.**
  - `(service_id, version)` is known before the insert, so a version is sealed and inserted in one step, and the table is fully immutable.
  - Registries are keyed by `host`, which is never edited.
  - A ciphertext copied to another service, version or host does not open.
- **Ciphertext.**
  - One JSON document per version: `{"v":1,"vars":[{"name","value","secret"}]}`. **Plain values are encrypted too.**
  - The `secret` flag in the document is authoritative. `names_json` is for display only.
- **Snapshot v2.** The v1 fields, plus:
  - `env_version` and `env_version_id`;
  - effective `strategy` and `strategy_forced`;
  - `mounts`, as `[{kind, source, target, read_only}]`;
  - `memory_mb`, `cpus_milli` and `no_new_privileges`.

  `ParseSnapshot` reads v1 (as v2 with nothing set) and v2. It writes only v2.
- **Retention.**
  - Kept per service: the last 20 env versions, plus the current one and every version that a queued, running or active deploy references.
  - Pruning runs lazily on write.
  - `RESTRICT` protects the current version.
  - A rollback to a pruned version is refused with 409.
  - Worst case: 21 × 24 KiB × 100 services ≈ 50 MB.
- **`secretbox.Columns()`** gains both new ciphertext columns.
- **Lost or wrong master key.**
  - App ciphertexts exist and the key file is **missing or does not match the recorded id**: the start and `admin reset` refuse, unless `admin reset --forget-app-secrets` is used.
  - That flag first lists what is lost: each service's env versions, plain values included, and each registry credential. Then, in one transaction, it:
    - sets `services.env_version_id` to NULL and flags `needs_attention = env_lost`;
    - deletes the env versions and the registries' `username` and `secret`;
    - lets `admin reset` continue.
  - `env_lost` refuses deploy, redeploy and rollback with: "Its environment variables were lost with the master key; save them again before deploying". A new env version clears it. The live container keeps running.
  - With the right key present, app secrets are never touched.
  - `LoadOrCreate` messages are reworded: "restore the key from a backup; `serverdock admin reset` clears the admin sign-in data, and with `--forget-app-secrets` also deletes app environment variables and registry credentials the lost key protected".
- **Fixture:** `internal/store/testdata/v5.db`, with an upgrade test from `v4.db`.

### Docker client and agent ops (ADR 0012 addendum A)

General rules:
- All new ops are `LocalOnly` and v1, decoded strictly, with capped results.
- Neither the hub nor the agent logs op arguments.
- The `auth` field and `Env` values never appear in any agent error, log line or result. Create errors name only keys and positions.

| Op | Args | Result | Rules |
|---|---|---|---|
| `image_pull` (changed) | optional `auth` (≤ 4 KiB, base64url `X-Registry-Auth`) | unchanged | sets the header on this request only; never echoed; redacted by value from agent errors |
| `container_create` (changed) | spec may carry `Env`, `Mounts`, limits, `SecurityOpt` and a second endpoint | unchanged | re-resolves each bind source (`EvalSymlinks`) right before create, re-checks it, and puts the resolved path in the body; then `CheckCreateAgent` |
| `volume_ensure` | `name`, `service` | `{name, created}` | `local` driver, no options, our labels (`managed`, `instance`, `service`); idempotent; refuses a volume with another driver, any option, or not our instance and this service id |
| `volume_remove` | `name`, `service` | `{}` | only with our instance label **and `serverdock.service` equal to `service`**; `force=false`; "in use" comes back as `conflict` |
| `project_network_ensure` | `name`, `subnet`, `project` | `{id}` | internal bridge, explicit subnet, our labels; idempotent; a 409 on create leads to inspect and compare; refuses foreign labels, another subnet, not internal, or a gateway mode other than absent/`nat` |
| `network_remove` | `name` | `{}` | only our labelled project networks; never forced |

**`dockerapi` gains:**
- `VolumeCreate`, `VolumeInspect`, `VolumeRemove`;
- `NetworkRemove`;
- network labels in the network list (`NetworkSubnets` returns only id, name and subnets today; `reconciler.remove_network` needs the instance and `serverdock.project` labels);
- a project-network create;
- the auth header on `ImagePull`;
- `Info` extended with `MemTotal`, `NCPU`, `DockerRootDir` and the version string.

**`CreateSpec` gains** typed `Env`, `Mounts`, `Memory`, `MemorySwap`, `NanoCpus`, `SecurityOpt` and a second `EndpointsConfig` entry with `Aliases`. API 1.44 and later accept several endpoints at create.

**`NetworkEnsure` refuses a gateway mode other than `nat`.**
- A network with `…gateway_mode_ipv4` or `…_ipv6` set to anything else gets `ErrNetworkSettings`.
- **Recovery** for an M3a install whose app network was changed. New deploys refuse; running apps keep serving. The docs give the steps:
  1. stop ServerDock;
  2. `docker rm -f $(docker ps -aq --filter label=serverdock.instance=<id>)`;
  3. `docker network rm serverdock-<id8>`;
  4. start ServerDock, which recreates the network with the recorded subnet;
  5. redeploy each service.

  This means app downtime, and the docs say so.

**ADR 0012 addendum A (step 2):**
- Decision 3 gains "plus one internal network per project for service DNS, joined at create time".
- New ops for volumes and project networks, with volumes bound to their service id.
- Registry credentials travel in one op argument and are never stored on the host.

### Policy (`internal/policy`): what `CheckCreate` allows now

Default-deny to the leaves, in both processes. The control plane checks exact values; the agent checks shape.

| Path | Control plane expects | Agent checks |
|---|---|---|
| `NetworkingConfig.EndpointsConfig` | exactly two keys: the app network `{IPAMConfig: {IPv4Address}}` (M3a), and the project network `{Aliases: ["<service name>"]}` | the app entry as in M3a; at most one more key, named `serverdock-<install id[:8]>-p<N>` with **N equal to the container's `serverdock.project` label**, holding only `Aliases` with one element that passes `ValidateName` |
| `Env` | exactly the built list, in order | strings `NAME=value`; valid names; no duplicates; no NUL; the four `SD_` names exactly once; **encoded size within the env cap** |
| `HostConfig.Mounts` | exactly the built list | ≤ 10 entries `{Type, Source, Target, ReadOnly}`; binds may add `BindOptions: {Propagation: "rprivate"}`. `volume`: Source `sd-<install id[:8]>-s<N>-<name>` with **N equal to the `serverdock.service` label**. `bind`: Source passes the bind rules. No `VolumeOptions`, `TmpfsOptions`, `Consistency` or `CreateMountpoint`. Target passes the target rule |
| `Memory`, `MemorySwap` | both `memory_mb × 1 MiB`, or both absent | equal and ≥ 32 MiB, or both absent |
| `NanoCpus` | `cpus_milli × 10⁶`, or absent | ≥ 5 × 10⁷, or absent |
| `SecurityOpt` | `["no-new-privileges:true"]` when recorded, else absent | that value only, or absent |

Still refused: `Binds`, `VolumesFrom`, `Tmpfs`, `OomKillDisable`, `OomScoreAdj`, `CpuShares`, `CpusetCpus`, `MemoryReservation`, `KernelMemory`, `StorageOpt`, `Devices`, `Links`, and the rest of the M3a list.

**Env rules (fuzzed).**
- **Names:** `^[A-Za-z_][A-Za-z0-9_]{0,127}$`. `SD_` is reserved in any case.
- **Values:** UTF-8; no NUL; C0 controls only `\t`, `\n`, `\r`.
- **Count:** ≤ 100 variables.
- **Secret values** are at least 8 bytes.
- **Env cap.** The cap is **the byte length of the `Env` array exactly as `json.Marshal` encodes it in the `container_create` request line**.
  - That is the hub's request encoder. HTML escaping is on, so `<` costs 6 bytes.
  - The length includes the context variables.
  - **Cap: 24 KiB.**
  - `env_set` and `services.create` measure it with the same function, assuming the context variables at their maximum length. The agent measures the received array the same way.
  - The whole request line, with 10 mounts, labels, limits and both endpoints, stays ≤ `agent.MaxRequestLine` (64 KiB). Raising that limit would be a protocol change and is not done here.
  - **Worst-case test (step 4):** 100 variables whose values repeat `<"\n\t` up to the cap, the longest names, 10 mounts at the longest paths, every limit, and no-new-privileges. The encoded line must be ≤ `agent.MaxRequestLine`.
  - **Memory per deploy:** a few copies of ≤ 24 KiB of plaintext, under 200 KiB per job, so under 400 KiB with 2 workers.

**Mount targets:** absolute, `path.Clean`-canonical, ≤ 255 bytes, printable ASCII with no spaces or `:`. Not `/`, and not under `/proc`, `/sys` or `/dev`.

**Volume names:** `^[a-z][a-z0-9-]{0,29}$`. The Docker name is `sd-<install id[:8]>-s<service id>-<name>`.

**Bind mounts.**
- **Denied set.** These are real paths, resolved with `EvalSymlinks` at start:
  - `/` itself, as a root or a source;
  - `DATA`;
  - the Docker socket's parent directory;
  - Docker's `DockerRootDir` from `Info` (if `Info` has not been read yet, bind mounts are refused at save and at the check stage rather than checked against a partial set);
  - `/var/lib/containerd`, `/run`, `/var/run`;
  - `/proc`, `/sys`, `/dev`, `/boot`, `/etc`, `/usr`, `/bin`, `/sbin`, `/lib`, `/lib32`, `/lib64`, `/libx32`, `/root`;
  - the ServerDock binary's directory.
- **Bind roots.** Set with `--bind-root DIR`, which can be repeated. There is none by default, so bind mounts are off. The server refuses to start when a root:
  - is not an existing directory after `EvalSymlinks`;
  - **is equal to, an ancestor of, or under any denied path**;
  - **has any "other" permission bit set**;
  - **is owned by anyone other than root or ServerDock's user**;
  - **is given while ServerDock runs in a container** (`/.dockerenv` or `/run/.containerenv` exists).
- **A bind source:**
  - is absolute and canonical, ≤ 255 bytes, **printable characters only**, with no `..`;
  - **after `EvalSymlinks`, must be an existing directory under a root, and neither equal to, an ancestor of, nor under any denied path.** This is checked at save, at the check stage, and again in the agent right before create.
  - **Docker receives the resolved path.**
- **No overlapping sources, in any service, including the same service.** No two bind sources may be equal to, an ancestor of, or under one another, using freshly resolved paths. Checked at save and again at the check stage, against the current lists of every service plus the mounts recorded by queued and running deploys (security round 2).
- **Options:**
  - `rprivate` propagation, and no mountpoint creation.
  - Docker applies `nosuid` only if the root's filesystem is mounted that way, so the docs advise `nosuid,nodev` for bind roots.
  - On kernels before 5.12, `read_only` is not recursive. The docs say so.
- **Accepted, in plain words:** between the agent's re-resolve and Docker's mount, the directory could be swapped for a symlink. The actor who could do that is **a container with a writable bind covering that path** (removed by the overlap rule, across services and within one service), or a host user who can write inside the bind root. Both are covered in the new A5 row.

**Limits.**
- `memory_mb`: 32 up to the host's `MemTotal`.
- `cpus_milli`: 50 up to `NCPU × 1000`, in steps of 50.
- Host facts come from the control plane's in-memory `Info`, checked at save and at the check stage.

**Registry addresses.** `CheckRegistryAddrs(host, addrs, allowPrivate)` **refuses if any resolved address is blocked.**
- **Always refused:**
  - link-local and metadata addresses;
  - `0.0.0.0/8`, `240.0.0.0/4`, `255.255.255.255`;
  - multicast;
  - M3a's translation forms.
- **Loopback** needs `--allow-loopback-registry` only; `allow_private` does not cover it.
- **Refused unless the host's entry has `allow_private`:**
  - RFC 1918;
  - CGNAT;
  - ULA;
  - `198.18.0.0/15`;
  - **every address on the host's own interfaces**, including `docker0`, `br-*` and ServerDock's gateways.
- `docker.io` is resolved as `registry-1.docker.io`.
- The `DialContext` check stays in M5.

### Environment variables and secrets

- **`services.env_set {service, base_version, vars: [{name, value?, secret, keep?}]}`**
  - Every `value` is tagged `sd:"secret"`. That is harmless for plain values: they are only kept out of audit rows.
  - **`keep: true` requires `secret: true`** and an existing secret of that name in the current version.
  - **`base_version`** must equal the current version number. Otherwise 409: "the variables were changed since you opened them; reload".
  - **Pointer rules, after `keep` is resolved:**
    1. The set equals the current version: nothing is written.
    2. Otherwise, if it equals the live deploy's version: the service points back at that version id.
    3. Otherwise: a new version is sealed and inserted, the service points at it, and old versions are pruned.
  - Returns `{version, applied, notice}`. `applied` means the current version is the live one.
  - Clears `env_lost`.
- **`services.env_get`** returns:
  - the current and live version numbers;
  - each variable's name and authoritative flag;
  - **values of non-secret variables only**.
- **`services.env_reveal {service, name}`** returns one secret of the current version.
  - It is `Sensitive`: fresh 2FA, tokens and MCP denied, intent row first.
  - The response is `no-store`.
  - The UI shows the value until Hide or until the user leaves the page.
- **Decryption** happens only in `env_get`, `env_reveal`, `env_set` (to compare and to resolve `keep`), the check stage and `secretsFor`. Nothing is cached.
- **Context variables** are appended after the user's:
  - `SD_DEPLOY_ID=<deploy id>`;
  - `SD_ENV=production`;
  - `SD_FQDN=` (empty until M4a);
  - `SD_COMMIT=` (empty until M5).
- **Docker holds env in clear.** This is accepted. ServerDock's ops never return `Env`.

### The secret redactor

- **Built on `redact.New`.** `redact.ForSecrets(values...)` expands each value into these forms, then calls `New`:
  - the value itself;
  - for multi-line values, each `\n` or `\r` piece of 8 bytes or more;
  - `agent.CleanLogText(value)`;
  - the JSON-escaped form;
  - the `%q`-escaped form.

  The 8-byte floor is enforced at input (`env_set`, `services.create`, `registries.set`), not in `New`.
- **Streaming:** `(*Redactor).Writer(w)` holds back (longest form − 1) bytes across chunks and flushes on `Close`.
- **Order:** reasons and details are redacted **before** they are truncated to 512 or 256 bytes.
- **Scope:** **`deploy.secretsFor(deployID)`** gives the secrets of the deploy's env version and of the service's current version, plus the registry password for the image host. It is used by:
  - the runner (reasons, details, job errors);
  - the deploy-log keep, on the runner and reconciler paths;
  - `deploys.log` reads;
  - the `error` column of `jobs.run_*`.
- **Fail closed.** If `secretsFor` fails:
  - the log is not kept ("log not kept: secrets unavailable");
  - `deploys.log` answers 503 "the log cannot be shown safely right now";
  - reasons and details fall back to a fixed text that names the stage.
- **What it cannot catch:**
  - other transformations of a secret (base64, URL encoding, short substrings);
  - secrets the app generates itself.

  The accepted risk on deploy logs is narrowed accordingly.
- **`deploys.log` reads are audited (QM3b-5).**
  - The new flag **`AuditRead`** writes an intent row before the read and an outcome row after. There is no 2FA prompt.
  - **If the intent write fails, no log is returned** ("audit unavailable; log not shown"). This includes a full disk.
  - `deploys.log` keeps `write` scope. It is not `Sensitive`.

### Registry credentials

- **`registries.list`** (read, `write`): host, username, `allow_private`, whether a secret is set, and `updated_at`.
- **`registries.set {host, username?, secret?, keep_secret?, allow_private}`** (mutate, `admin`): validates the host, resolves it, checks the addresses, and seals `secret` (tagged `sd:"secret"`).
- **`registries.delete {host}`** (mutate, `admin`).
- **Per pull:**
  1. normalise the host;
  2. resolve it and re-check its addresses (5 s bound; a failed lookup is transient);
  3. open the secret;
  4. build `X-Registry-Auth` from `{"username","password","serveraddress"}`;
  5. pass it in that one `image_pull`.

  Digest pulls do the same. There is no `docker login`. Step 2 records the `serveraddress` that Docker Hub accepts.
- **Accepted leftovers:**
  - the token `realm`;
  - DNS rebinding;
  - a daemon-side `HTTPS_PROXY` or `registry-mirrors` setting, with which the daemon connects somewhere the address check never saw;
  - blob redirects.

### Volumes

- **`services.set_volumes`** (mutate, `admin`, **`Sensitive`**).
  - A `bind` entry needs a bind root and must pass the bind rules.
  - A removed entry keeps its row with `detached_at` set, and the Docker volume stays.
  - Re-adding the same name reattaches it.
  - The change applies at the next deploy.
- **Check stage:** `volume_ensure` for each named volume in the snapshot, and `Mounts` built from the snapshot.
- **`services.delete` gains `delete_volumes`** (default false).
  - It travels in the job payload (`EnqueueDelete`, `RunDeleteIn`).
  - "Delete again" asks the question again.
  - Order: route away → containers → **volumes**, including detached rows → `deleteRows`.
  - A volume in use does not fail the delete. The output and the audit outcome say "volume sd-…-data kept: in use by another container".
  - Bind host directories are never deleted.
  - With `delete_volumes` false, the output lists the volumes that were kept.
- **Stop-first is forced by a writable recorded mount.** The M3a guarantees hold. An interrupted stop-first candidate still has at most 10 s of stop grace, and the Storage screen says so.

### Per-project network and service DNS

- **Decision (B1, needs `systems` sign-off): the candidate joins the project network at create time, with its service alias.**
  - **Joining after the health gate** (the round-1 design) means a candidate cannot reach `db` while it is being checked, so apps that connect at start fail every deploy.
  - **Joining without an alias and adding the name later** does not work cleanly. Docker cannot add an alias to an existing endpoint, so it would take a disconnect and reconnect. That **cuts the candidate's connections to its peers** right before it goes live: apps that exit on a lost database connection would then fail after the switch. It also adds a Docker call, with its own failure path, between the health gate and the switch.
  - **The cost of joining with the alias.** During a blue-green deploy, a peer looking up `api` can get the new version's address from its start, before its health gate passes.
    - Stop-first services never overlap. That includes every service with a writable volume, so every database.
    - An unhealthy candidate is removed within `health_timeout_s`, and its name goes with it.
    - Docker's DNS returns both addresses in varying order, so a client may reach either version until the candidate's gate fails or the switch happens.
    - A second network (no alias at create, aliases on a network joined after the gate) does not help: the candidate can resolve `db` during its gate only if db's alias is on a network it already joined, which is the same "answers the name early" cost, plus a second /25 per project, the attach stage again and one more resource to clean up.
    - Goal 5, the Network section of the UI and architecture.md all say this.
  - **The gain:** no new stage, no new failure path, no connection cut, and no change to the commit order or the boot pass.
- **Create body:** two endpoints. Docker also registers the container name (`sd-<project>-<service>-d<id>-a<n>`) on the project network. It reveals nothing beyond the service name.
- **Failures are create failures.**
  - If create reports the project network missing, the runner runs `project_network_ensure` once and retries the create once.
  - Any other failure is `start_failed`:
    - **blue-green:** the candidate is removed and the old version is kept (`app_safe` yes);
    - **stop-first:** `restoreOld`, ending `app_safe` yes or `previous_version_down`. The restarted old container keeps its project endpoint, because stopped containers keep their endpoints (tested).
  - No new stage, so `interrupted.go` gets no new rule. kill -9 at `start` with two networks is added to the suite.
- **Subnet allocation.** In one `WriteTx`:
  1. read every `projects.subnet`;
  2. take Docker's networks (`NetworkSubnets`, read just before), the host's interfaces and routes, and `meta.app_subnet`;
  3. pick the **first /25 of `10.250.0.0/16`** that overlaps none of them. `--project-pool CIDR` is an escape hatch that replaces the range for new allocations only;
  4. `UPDATE projects SET subnet = ? WHERE id = ? AND subnet IS NULL`. The partial unique index catches a race; the allocation retries once.
  - A later overlap refuses deploys **for that project only**. Running apps are unaffected.
  - **Nothing free:** that project's deploys fail with "no free address range for project shop's network in 10.250.0.0/16; free one, or start ServerDock with --project-pool". Other projects are unaffected.
- **Isolation, with Docker's firewall rules on.**
  - Within a project: services reach each other by name, on any port.
  - Across projects: no shared network that allows container-to-container traffic. The app network keeps `enable_icc=false`.
  - The host and public ports stay reachable (B10).
  - The project network is `Internal`, so it adds no egress, no NAT and no outside route.
  - Step 2 records whether an internal network's gateway reaches the host.
- **Removal.**
  - `deleteRows` reports, from inside its own transaction, whether it deleted the project row. The network is removed only then, after the commit.
  - A new service in the same project keeps the row, so the network stays.
  - A network with foreign endpoints is left in place and logged.
  - **Resync rule**, part of the full resync on each events (re)connect: a network with our instance label and a `serverdock.project` that has no row is removed without force, through **`reconciler.remove_network`**. Only when the name is exactly `serverdock-<id8>-p<N>`, it carries the `managed` and instance labels, and `serverdock.project` is a canonical decimal equal to N. The networks are listed *before* the project rows are read, so a project created during the resync is never an orphan; if the project query fails, the rule is skipped. It is listed under "Unattended destructive actions" in step 9.
  - **Accepted:** someone can `docker network rm` the network of a project whose containers are all stopped. The next deploy recreates it. This is documented.
- **Upgrade from M3a:** a service gets its project endpoint at its next deploy. The release notes say so.

### Resource limits and OOM reporting

- **`services.set_resources {service, memory_mb?, cpus_milli?, no_new_privileges}`** (mutate, `admin`). Applies at the next deploy through the snapshot.
- **Defaults:** no limits (QM3b-3). The form shows the host's totals and a warning about the kernel killing ServerDock.
- **no-new-privileges** is off by default.
- **OOM, reported in three places, with no timer:**
  1. A candidate killed during its health gate fails `oom`, and the reason names the limit.
  2. An `oom` event writes **`services.last_oom_at`**.
     - At most one write per service per minute, decided by an in-memory timestamp.
     - The write runs in the existing events goroutine (no new goroutine); the timestamp map entry is pruned when the service is deleted (unit test `TestOOMCoalesceMapPrunedOnServiceDelete`).
     - The event is already in the watcher's filter; `TestEventsFilterUnchanged` keeps it there.
     - Docker clears `OOMKilled` on restart (step 2 confirms), so inspect is not used to recover a missed OOM.
     - **An OOM during an events gap can be missed.**
  3. Overview "Needs attention": "A process in shop/api ran out of memory at 14:02 UTC (memory limit 256 MB)". It makes no claim that the container restarted. It shows until the next successful deploy, or for 24 h, decided when it is read.

### Docker engine version and host facts

- **One source:** the control plane's direct `dockerapi.Client.Info` and `/version` calls, read on every successful events (re)connect.
  - Facts kept **in memory**: engine and API version, `MemTotal`, `NCPU`, `DockerRootDir`.
  - A failed `Info` is logged and never fails the resync.
- **Lenient parse:** major and minor are read from the start of the string. An unknown format makes no pre-28 claim ("Docker version not recognised").
- **Stored:** `meta.docker_engine_version` is the last acknowledged version, recorded silently on the first run.
  - A change shows "Needs attention", with **Acknowledge** (`system.docker_ack`: mutate, `admin`, audited).
- **Before 28.0.0:** a standing warning on the Overview Server card and once in the startup log, using research 06's wording. Such engines are still accepted (QM3b-9).
- **`system.docker` (changed)** adds the acknowledged version, `pre_28`, `changed`, `mem_total_mb` and `ncpu`. The OpenAPI and web types are regenerated.

### Docker RC CI entry

- **What it does:** a second `integration` entry, `docker: rc`.
  - Removes the runner's preinstalled `moby-*` and `containerd` packages.
  - Installs the newest Engine from Docker's `test` apt channel, with the key pinned by fingerprint.
  - Records `docker version` in the summary.
  - Runs the same test command.
- **When it runs:**
  - nightly (a CI schedule, not a product timer);
  - on release branches;
  - on PRs that touch `internal/agent/dockerapi`, `internal/agentops` or the Docker workflow.
- **Only the stable entry is a required check.** At each release the RC must be green or have a written Docker-bug note; **the maintainer checks it** in the release PR. This amends plan 0002's exit item "RC matrix green" (QM3b-6).
- Rule 11 and CODEOWNERS apply.

### Actions added or changed in the registry

The env and registry actions are built in steps 5 and 6 but **registered on the server only in step 7**, once deploys use them.

| Action | Risk | Scope | Flags | Notes |
|---|---|---|---|---|
| `services.env_get` | read | **write** | — | flags from the decrypted document |
| `services.env_set` | mutate | **admin** (QM3b-1) | — | `base_version`; `sd:"secret"` values; `keep` only for secrets |
| `services.env_reveal` | read | admin | **Sensitive** | |
| `services.create` (changed) | mutate | admin | — | optional `env` (`sd:"secret"` values); service and env version 1 in one transaction, then enqueue if `deploy:true` |
| `registries.list` | read | write | — | |
| `registries.set` | mutate | admin | — | |
| `registries.delete` | mutate | admin | — | |
| `services.set_volumes` | mutate | admin | **Sensitive** | detached rows kept |
| `services.set_resources` | mutate | admin | — | |
| `services.delete` (changed) | destructive | admin | — | `delete_volumes` in the payload |
| `services.rollback` (changed) | mutate | write | — | a `write` token only to the **current** env version; otherwise `admin`, checked in the enqueue transaction |
| `services.get`, `services.list` (changed) | read | read | — | `pending_changes`, `effective_strategy`, `dns_name`, `last_oom_at`, limits, `env_lost` |
| `deploys.get` (changed) | read | read | — | env version, mounts, limits |
| `deploys.log` (changed) | read | write | **AuditRead** | redacted; fails closed |
| `system.docker` (changed) | read | read | — | new fields |
| `system.docker_ack` | mutate | admin | — | |
| `reconciler.remove_network` | destructive | admin | ConsoleOnly, `System` = "crash reconciliation: remove a project network labelled with this install whose project no longer exists (never forced)" | listed in `docs/security.md` |

- `AuditRead` requires `Risk = Read`, otherwise `Register` panics.
- MCP never gets `env_get`, `env_reveal` or `deploys.log`.
- OpenAPI and the web types are regenerated, and the route matrix is extended.

### UI (SPA)

1. **Service › Environment** (new). The sub-navigation becomes Deploys · Environment · Settings.
   - **Rows:** Name; Value, or "Hidden" with **Reveal**; a Secret checkbox; Remove. A table at ≥ 768 px, stacked cards below.
   - **Save** sends `base_version`. A 409 shows "Changed elsewhere; reload" and keeps the typed input.
   - After saving: "Saved as version 7. Deploy to apply." with **Deploy now**, which calls `services.redeploy` when something is live and `services.deploy` otherwise.
   - A "Not deployed yet" line.
   - An `env_lost` banner.
   - Read-only context variables ("Set by ServerDock").
   - **Reveal** goes through the step-up dialog, then shows a read-only field with Hide and Copy, and announces "Value shown" once.
   - **Empty state:** a hint and a docs link.
2. **New service:** an optional **Environment variables** disclosure, sent in `services.create`.
3. **Service › Settings:**
   - **Resources:** memory in MB with the host total, CPU in steps of 0.05 with the host's CPUs, and the no-new-privileges switch with its explanation.
   - **Storage:**
     - the list of mounts;
     - Add volume, and Add bind mount (shown only when a bind root exists; otherwise "Bind mounts are off on this server" with a docs link);
     - saving needs fresh 2FA;
     - notices: the M6 backup notice, the 10 s stop-first note, and the kernel note on recursive read-only.
   - **Network** (read-only): "Other services in project shop reach this service at `api`. During a blue-green deploy, the name also answers from the new version until the old one stops."
   - **Strategy:** shows the forced stop-first with its reason, `aria-disabled`.
4. **Delete service dialog:**
   - the volume list, detached ones included;
   - "Also delete these volumes and their data", unchecked by default;
   - the result shows "kept: in use".
5. **Rollback dialog:**
   - names the env version it will run;
   - **warns** when that version differs from the current one ("This brings back the variables of version 4, including secrets you replaced since");
   - **warns** when the target ran with no variables (a pre-M3b deploy);
   - for a `write` token, shows the refusal with its reason.
6. **Other screens:**
   - **Settings › Registries:** list, Add/Edit (password, with "keep the saved one"), the allow-private warning, Delete with a confirmation, and an empty state.
   - **Overview:** the Server card (versions and the pre-28 warning), plus "Needs attention" entries for a version change (with Acknowledge) and for an OOM.
   - **Deploy detail:** env version, limits, mounts, and the OOM reason.
   - **Services list:** a memory column and the state word.

**Accessibility (plan 0002 rule 6; M2 and M3a rules carry over):**
- Every row control's name includes the variable name ("Reveal DATABASE_URL").
- Errors appear in the error summary, linked to their row.
- Reveal never moves focus; Hide returns focus to Reveal.
- Disabled or forced controls give their reason in text.
- States are shown as a word plus a symbol.
- 44 px targets, reflow at 320 px, both themes, forced colours, reduced motion.
- No polling is added.

### Planner defaults (the maintainer may change any)

- Env: 100 variables, 128-character names, a 24 KiB cap, secrets of at least 8 bytes, 20 versions kept.
- 10 mounts.
- Limits: memory at least 32 MB; CPU at least 0.05, in steps of 0.05; no default limits; no-new-privileges off.
- A /25 per project from `10.250.0.0/16`.
- Bind mounts off.
- OOM entries shown for 24 h.
- The RC entry is not a required check.

## Footprint rules for new Go code (S4)

- **No package-level work** (`sync.OnceValue`; the `nopkginit` test).
- **Agent mode runs none of it.**
  - The remote op table stays `{hello, logs}`.
  - The remote Caddy config is byte-identical (golden test).
- **No new dependency.**
- **No new timers, tickers or idle goroutines.** The new work runs only inside requests, jobs or the existing (re)connect:
  - decryption and the redactor;
  - registry lookups;
  - the `Info` read;
  - volume and network ensures;
  - the engine comparison;
  - the OOM write (on the event, coalesced);
  - env pruning (on write);
  - the network orphan rule (in the full resync).

  The events filter is unchanged (`TestEventsFilterUnchanged`).

## Footprint budget

**Allocation:**
- S2 ≤ 58 MB.
- **S4 46.7 MB, settled by plan 0002's re-base rule:** 47.8 − (47.6 − 46.5). The plan 0002 table is edited in the reorder PR.
- Ceilings 200 / 100 MB; tripwires 80 / 50 MB.

**Baseline:** PR #6 CI.

| | M3a CI | M3b estimate | Planning number | Cost |
|---|---|---|---|---|
| **S2 live watcher** | 54.0 MB | **≈ 54.5–55.5 MB** | ≤ 58 (tripwire 80) | code pages (≈ 0.3–0.6 MB), tables; facts from `Info` (bytes) |
| **S4** | 46.5 MB | **≈ 46.5–46.8 MB** | 46.7 (tripwire 50), judged by full-run median plus cause | file-backed pages of new `dockerapi` code |
| Idle CPU | 0.00 % | 0.00 % | < 1 % / < 0.5 % | none |
| Peak per deploy job (cached image) | +0.3 MB (S7) | + ≤ 2 MB per job, **≤ 4 MB with 2 workers** | ≤ 5 MB | env (≤ 200 KiB per job), redactor hold-back (< 4 KiB per stream), `Info` |
| **`deploys.log` read**, 1 MiB log, 100 secrets × 5 forms | ≈ 3.1 × log | measured in step 7; ≈ 3.5 MiB per read, **≤ 7 MiB with the 2-read cap** (the existing M3a limiter `MaxLogReads` in `internal/deploy/deploylog.go`, a 2-slot channel with no goroutine; step 7 keeps it in front of the redaction pass and tests that a third concurrent read waits or is refused) | report | one extra pass |
| **S10 cold pull** | — | step 15 | at 60 s: RSS within **+2 MB** of idle; goroutines equal; `HeapInuse` within 1 MB; peak and VmHWM reported | pull parsing |
| **S11 leak loop** | — | step 15 | after 200 redeploys: RSS within **+2 MB** of the warm baseline; empty goroutine-dump diff; `HeapInuse` within 1 MB | — |
| Binary (gzip) | 20.0 MB | ≈ 20.3 MB | ≤ 40 MB | code |
| SPA JS | 41.5 / 115.0 KB | ≈ 43 / 140 KB; per-route chunks reported in steps 13–14 | ≤ 250 KB first load | three screens |
| Disk | — | env versions ≤ 21 × 24 KiB per service | — | volumes are user data |

**S10 method** (local bench):
- Two images in the local `registry:2`: about 100 MB with **30+ layers**, and one with **a single 100 MB layer**.
- The server runs with `--allow-loopback-registry`.
- Before each run: remove the image, and reset VmHWM (`echo 5 > /proc/<pid>/clear_refs`).
- Sample RSS every 100 ms through the deploy and for 60 s after.
- Goroutines and `HeapInuse` come from `bench/idle-memory.sh`.

**S11 method** (manual or nightly, about 40 minutes):
- One service with env, a **read-only** volume (so it stays blue-green and fast) and limits.
- A warm baseline after the first redeploy.
- A checkpoint every 20 deploys: RSS, goroutines, and `HeapInuse` after a `FreeOSMemory` checkpoint through the debug socket.
- A goroutine dump before and after, diffed.
- Check that `last_oom_at` and the coalescing map stay bounded.
- Record the runtime.

**Raises idle RAM?** Only code and schema (≈ 0.5–1 MB on S2). No new background work.

**When things are measured:**
- CI bench on every Go PR.
- QUICK S4 at steps 2 and 3.
- **Step 4: QUICK S4, an idle `/debug/goroutines` diff, then a mandatory full S4 run.**
- S2 idle goroutines and RSS at steps 10 and 11.
- At step 15: full S4, S2, S5, S6, S7 with a configured service, S10, S11 and the `deploys.log` peak.

**Dependencies:** none at runtime. Test-only:
- test-app flags `-print-env`, `-alloc-mb`, `-write-file`/`-check-file`, `-resolve NAME`;
- `registry:2` with a generated `htpasswd`;
- the S10 images.

## Security notes

- **Secrets:** env values and registry passwords live in secretbox, AAD-bound. A lost or wrong key goes through `--forget-app-secrets` and `env_lost`.
- **Inputs (fuzzed):**
  - env names and values, `keep`, `base_version`;
  - registry hosts and usernames;
  - bind paths and roots, mount targets, volume names;
  - limits;
  - the new op arguments;
  - the wider create body.
- **Permissions:**
  - plain env: `write`;
  - env changes, registries, volumes, resources: `admin`;
  - reveal and storage: `Sensitive`;
  - `write` tokens can roll back only to the current env version.
- **Bind mounts:** see Policy › Bind mounts. **New A5 row (step 8):** "a local user swaps a directory inside a bind root for a symlink". Mitigated by the owner and "other"-bit rules on roots and by the agent's re-resolve. The remaining window is accepted.
- **Named volumes:** `local` driver only, no options; bound to their service id at create and at removal.
- **Registry credentials:** in-process only, sent as one header on one pull. The M3a registry-host deferral is closed, with the leftovers stated.
- **Redaction:**
  - covers exact values in several forms, streams, and fails closed;
  - scoped to one deploy;
  - `deploys.log` reads are audited.
- **New accepted risk: any admin-scope credential (an admin API token, or a session without fresh 2FA) can read every secret without using reveal.** `services.set_source` can swap in an image that prints its environment, and redaction is exact-match only.
  - Reveal's 2FA and audit therefore protect against stolen `write` tokens and casual viewing, not against an admin.
  - QM3b-11 (accepted): `set_source`, image changes through `update`, and rollback to another repository need fresh 2FA and deny tokens for services with a bind mount.
- **Docker shows env in clear:** accepted.
- **Project networks:**
  - reach is within the project, with Docker's firewall rules on;
  - a candidate answers the name early (B1);
  - `reconciler.remove_network` is listed.
- **Docker engine:** the pre-28 warning; the `nat-unprotected` refusal and its recovery steps.
- **Remote agents:** every new op is `LocalOnly`.

## Steps (each one PR-sized, in order)

1. **Schema, snapshot v2, secretbox columns, key rules.**
   - Do:
     - `0005` and `v5.db`;
     - snapshot v2;
     - `Columns()`;
     - the missing-or-wrong-key refusal, `--forget-app-secrets`, `env_lost`;
     - the `LoadOrCreate` messages.
   - Tests:
     - migrations fresh and from `v4.db`; `v5.db` opens; `TestProjectsSubnetUniqueOnlyWhenSet`;
     - `TestEnvVersionsAreImmutable`; `TestEnvCiphertextOpensOnlyForItsServiceAndVersion`; `TestRegistrySecretOpensOnlyForItsHost`;
     - `TestServiceVolumesChecksTieKindToNameOrPath`; `TestLimitsChecks`;
     - `TestSnapshotV1ReadsAsV2`; `TestSnapshotV2Validate`;
     - `TestMissingKeyWithAppSecretsRefusesStart`; `TestAdminResetRefusesWithAppSecretsAndNoKey`; **`TestAdminResetRefusesWithAppSecretsAndAWrongKey`**; `TestAdminResetForgetAppSecretsListsThenDropsAndFlagsEnvLost`; `TestAdminResetWithTheKeyNeverTouchesAppSecrets`;
     - `TestDeletingAServiceWithEnvVersionsSucceeds`; `TestCurrentEnvVersionCannotBeDeleted`.
   - Footprint: QUICK S4.
   - Reviewers: `reviewer`, `security` (required), `systems` (required), `footprint`.

2. **Docker client; ADR 0012 addendum A.**
   - Do:
     - the `dockerapi` additions, including two endpoints at create;
     - the `nat-unprotected` refusal and its recovery text;
     - the addendum.
   - Tests:
     - fake-daemon tests for each method; `TestPullSendsRegistryAuthOnlyOnThatRequest`; `TestPullErrorNeverEchoesTheAuthHeader`; `TestNetworkEnsureRefusesNatUnprotected`; `TestInfoParsesVersionsLeniently`;
     - **integration on stable and the RC:**
       - volumes, including one in use;
       - an internal network, and a two-network create resolving a peer's alias;
       - an authenticated `registry:2` pull (record the Docker Hub `serveraddress`);
       - **record:** whether Docker clears `OOMKilled` on restart; whether an internal gateway reaches the host; whether a stopped container keeps its endpoints.
   - Footprint: QUICK S4, binary delta.
   - Reviewers: `reviewer`, `security` (required), `systems`, `footprint`.

3. **Agent ops.**
   - Do:
     - `volume_ensure`, `volume_remove` (with the service id), `project_network_ensure` (a 409 means inspect), `network_remove`;
     - the `auth` argument on pulls;
     - the bind re-resolve at create;
     - keep Env values out of errors.
   - Tests:
     - every op; `TestVolumeEnsureRefusesForeignDriverOrOptions`; `TestVolumeRemoveChecksTheServiceID`; `TestProjectNetworkEnsure409InspectsAndCompares`; `TestNetworkRemoveNeverForces`;
     - `TestPullAuthNeverInAgentLogsOrErrors`; `TestCreateErrorsNeverEchoEnvValues`; `TestAgentSendsTheReResolvedBindPath`;
     - `TestRemoteAgentServesNoNewOp`; goleak.
   - Footprint: QUICK S4.
   - Reviewers: `reviewer`, `security` (required), `systems`, `footprint`.

4. **Policy.**
   - Do:
     - the `CheckCreate` additions;
     - the env cap;
     - the bind rules;
     - `CheckRegistryAddrs`;
     - `BuildCreate`.
   - Tests:
     - fixtures for each new refusal, including a project endpoint whose number differs from the label, two aliases, and a volume `s<N>` that differs from the service label;
     - **`TestWorstCaseCreateRequestFitsTheRequestLine`**;
     - **`TestBindRootRefusesAncestorsOfDeniedPaths`**; **`TestBindSourceUnderRootStillRefusesDataAndDockerRoot`**; **`TestOverlappingBindSourcesRefused`** (across services, within one service, and at the check stage); `TestBindRootRefusedInAContainer`; `TestBindRootOwnerAndModeRules`; `TestBindPathRules`;
     - `TestEnvRules`; `TestRegistryAddrRefusedIfAnyAddressIsBlocked`; `TestLoopbackRegistryNeedsTheFlagNotAllowPrivate`;
     - fuzzers: `FuzzCheckCreate`, `FuzzBindPath`, `FuzzMountTarget`, `FuzzEnvName`, `FuzzEnvValue`.
   - Footprint: **QUICK S4, an idle goroutine diff, then a mandatory full S4 run.**
   - Reviewers: `reviewer`, `security` (required), `footprint`.

5. **Env and secrets (built, not registered).**
   - Do:
     - the env store;
     - `env_get`, `env_set`, `env_reveal`;
     - `redact.ForSecrets` and its writer;
     - `secretsFor`;
     - `AuditRead`.
   - Tests:
     - `TestEnvSetEqualToCurrentWritesNothing`; `TestEnvSetEqualToLivePointsBack`; `TestEnvSetBaseVersionMismatchIs409`; **`TestEnvSetKeepRefusedUnlessTheNewEntryIsSecret`**; `TestSecretFlagComesFromTheDecryptedDocument`; `TestEnvGetNeverReturnsSecretValuesOrLengths`;
     - **`TestRevealWritesTheIntentFirstAndRevealsNothingWhenThatFails`**; `TestRevealNeedsFreshTwoFactorAndDeniesTokensAndMCP`;
     - **`TestRedactorCatchesASecretAcrossAChunkBoundary`**; `TestRedactorHidesLinePiecesOfMultiLineSecrets`; `TestRedactorHidesCleanedJSONAndQuotedForms`; `TestReasonsRedactedBeforeTruncation`; `TestSecretsForFailureKeepsNoLogAndRefusesTheRead`;
     - `TestRegisterRefusesAuditReadOnANonRead`; `TestAuditReadIntentFailureReturnsNoLog`.
   - Reviewers: `reviewer`, `security` (required).

6. **Registry credentials (built, not registered).**
   - Do:
     - the store and actions;
     - the resolver and the address checks;
     - the per-pull header.
   - Tests:
     - `TestRegistrySecretNeverReturnedOrLogged`; `TestPrivateAddressNeedsAllowPrivate`; `TestPullRechecksTheAddressAtEachPull`; `TestPullSendsCredentialsOnlyForTheMatchingHost`; `TestRollbackByDigestUsesTheCurrentCredential`;
     - integration: `TestPrivateRegistryDeployWithAuth`.
   - Reviewers: `reviewer`, `security` (required), `systems`.

7. **Runner; register the env and registry actions.**
   - Do:
     - the frozen snapshot v2; effective strategy and `timeout_s` from the recorded mounts; the runner builds only from the snapshot;
     - rollback refusals: pruned env, `env_lost`, a `write` token to another env version;
     - `services.create` with `env`;
     - redaction everywhere; `deploys.log` with `AuditRead`;
     - `pending_changes`;
     - registration, OpenAPI and types, route matrix.
   - Tests:
     - `TestRedeployTakesTheCurrentEnvVersion`; `TestRollbackTakesTheTargetsEnvVersion`; **`TestRollbackToBlueGreenIsStopFirstWhenTheServiceHasAWritableVolume`**; **`TestSetVolumesAfterEnqueueDoesNotChangeTheQueuedDeploy`**; `TestTimeoutDerivedFromTheEffectiveStrategy`; `TestQueuedM3aJobKeepsItsTimeout`;
     - **`TestWriteTokenRollbackToAnotherEnvVersionNeedsAdmin`**; `TestEnvLostRefusesDeployRedeployRollback`;
     - `TestPendingChangesComparesWithTheActiveSnapshot`; `TestContextVariablesAreSetAndCannotBeOverridden`; `TestCreateWithEnvIsOneTransaction`;
     - **`TestAppSecretsNeverLeakThroughDeployReads`**; `TestServicesCreateAuditRowsHoldNoEnvValues`; `TestDeployLogReadIsAudited`; `TestKeptLogOfAnOrphanIsRedacted`;
     - measure the `deploys.log` peak (reported).
   - Reviewers: `reviewer`, `security` (required), `systems` (required), `footprint`.

8. **Volumes.**
   - Do:
     - `set_volumes` with detached rows;
     - bind roots in the config and the startup log;
     - `volume_ensure` and mounts from the snapshot;
     - delete with `delete_volumes` (order, payload, "kept: in use");
     - the A5 row.
   - Tests:
     - `TestWritableVolumeForcesStopFirst`; `TestReadOnlyVolumeAllowsBlueGreen`; `TestBindRefusedWithoutABindRoot`; `TestSetVolumesNeedsFreshTwoFactorAndDeniesTokens`;
     - `TestRemovingAVolumeEntryKeepsTheDataAndTheRow`; `TestReaddingTheNameReattaches`;
     - `TestDeleteKeepsVolumesByDefault`; `TestDeleteVolumesIncludesDetachedOnes`; `TestDeleteAgainAsksForDeleteVolumesAgain`; `TestVolumeInUseIsReportedKeptAndTheDeleteFinishes`;
     - integration: `TestDataSurvivesARedeploy`; `TestVolumeTriggersStopFirstUnderLoad` (maintenance 503 only, zero Caddy loads); a bind mount under a temp root; kill -9 in a stop-first gap with a volume.
   - Reviewers: `reviewer`, `security` (required), `systems` (required).

9. **Project networks and service DNS.**
   - Do:
     - subnet allocation;
     - ensure at the check stage; two endpoints at create, with one ensure-and-retry;
     - removal after the row is deleted;
     - `reconciler.remove_network` and its security.md entry;
     - the blue-green DNS note in architecture.md.
   - Tests:
     - **`TestCandidateReachesPeersByNameDuringItsHealthGate`** (integration; the test app's `-resolve db` must succeed before it reports healthy);
     - `TestProjectSubnetAllocationIsAtomic`; `TestNoFreeSubnetRefusesOnlyThatProject`; `TestOverlapRefusesOnlyTheAffectedProject`; `TestMissingProjectNetworkIsEnsuredOnceAtCreate`;
     - **`TestStartFailureFromTheProjectNetworkKeepsTheOldServingBlueGreen`**; **`TestStartFailureFromTheProjectNetworkRestoresTheOldStopFirst`**; `TestRestoredOldContainerKeepsItsProjectEndpoint`;
     - **kill -9 at `start` with two networks** (blue-green and stop-first);
     - **`TestDeleteOfLastServiceRacesANewServiceDeploy`**; `TestResyncRemovesAnOrphanProjectNetworkWithoutForce`; `TestRemoveNetworkIsSystemOnlyAndListed`;
     - integration: `TestServicesResolveEachOtherByName`; **`TestOtherProjectsCannotResolveOrReach`** (both addresses, also on the RC); `TestUnhealthyCandidateLeavesTheNameWhenRemoved`; blue-green under load with zero non-2xx and zero Caddy loads.
   - Reviewers: `reviewer`, `systems` (required), `security`.

10. **Limits and OOM.**
    - Do:
      - `set_resources`;
      - limits from the snapshot;
      - `last_oom_at` from events, coalesced;
      - the Overview entry.
    - Tests:
      - `TestLimitsRefusedAboveTheHost`; `TestLimitsFrozenAtEnqueue`; `TestNoNewPrivilegesSwitch`;
      - `TestOOMDuringTheHealthGateNamesTheLimit`; `TestOOMEventWritesAtMostOncePerServicePerMinute`; `TestOOMEntryHiddenAfterADeployOr24h`; **`TestEventsFilterUnchanged`**;
      - integration: `TestOOMIsReported`; the CPU limit shows in the cgroup;
      - **S2 idle goroutines and RSS unchanged** (reported).
    - Reviewers: `reviewer`, `systems`, `security`, `footprint`.

11. **Docker engine version.**
    - Do:
      - the `meta` key;
      - facts read on reconnect;
      - `system.docker` (changed) and `system.docker_ack`;
      - the pre-28 warning;
      - log lines.
    - Tests:
      - `TestFirstRunRecordsTheEngineSilently`; `TestEngineChangeRaisesNeedsAttentionUntilAcknowledged`; `TestEngineChangeSeenAfterADaemonRestart`;
      - `TestPre28WarningOnlyBelow28AndNeverOnAnUnknownFormat`; `TestFailedInfoNeverFailsTheResync`; `TestAckIsAdminAndAudited`;
      - **S2 idle goroutines and RSS unchanged** (reported).
    - Reviewers: `reviewer`, `security`, `systems`, `footprint`.

12. **CI: Docker RC entry.**
    - Do:
      - the job and its triggers;
      - preinstalled packages removed;
      - the key pinned;
      - stable is the only required check;
      - plan 0002's exit wording amended;
      - a release-checklist line (the maintainer checks it).
    - Tests: runs on a Docker-path PR and is not required; the summary names the RC version.
    - Reviewers: `reviewer`, `security`, `footprint`.

13. **UI: Environment, New service variables, reveal.**
    - Do:
      - UI items 1–2;
      - Playwright fixtures;
      - report the chunk size of each route.
    - Screen states:
      - empty;
      - plain and secret variables;
      - not deployed yet;
      - field errors with the summary (bad name, reserved `SD_`, short secret, too large);
      - changed elsewhere (409);
      - `env_lost`;
      - the reveal step-up;
      - value shown;
      - save failed with the typed input kept.
    - Tests (all 8 projects):
      - zero axe violations;
      - set, save and deploy, then see a plain variable in the app (the test app echoes `/env/<name>`);
      - Reveal needs step-up; Hide returns focus to Reveal;
      - a DOM scan finds no secret value before Reveal;
      - no requests while idle;
      - keyboard-only;
      - 44 px targets;
      - no CSP violations.
    - Reviewers: `reviewer`, `accessibility` (required), `security` (required).

14. **UI: Settings, Registries, Overview, delete with volumes, rollback warnings, OOM.**
    - Do: UI items 3–6; report the chunk size of each route.
    - Screen states:
      - each section empty and filled;
      - bind mounts off;
      - storage saved but not yet deployed;
      - strategy forced;
      - Registries: empty, filled, errors, the private-address warning;
      - delete with and without volumes, and "kept: in use";
      - rollback warnings: env differs, no variables;
      - Overview: pre-28, version change, OOM;
      - Deploy detail failed with `oom`.
    - Tests (all 8 projects):
      - zero axe violations;
      - set a memory limit and redeploy;
      - add a volume and see the forced stop-first;
      - add and delete a registry;
      - Acknowledge clears the version-change entry;
      - delete keeps volumes when unchecked;
      - dialogs name the target and the consequence, default focus is on Cancel, Escape closes them;
      - no requests while idle.
    - Reviewers: `reviewer`, `accessibility` (required), `security`.

15. **Record and close.**
    - Do:
      - **`docs/security.md`:**
        - close row 154, with the leftovers;
        - rows 128 and 177;
        - asset 11;
        - the A5 bind row;
        - the accepted risks: limits off by default and no disk quota; narrowed deploy logs; Docker env in clear; the bind swap window and its actor; registry leftovers; project-network reach; B1's early resolution; admin reads every secret;
        - N13, now with its warning;
        - the unattended list, adding `reconciler.remove_network`;
        - the plan 0012 hand-off for MCP;
        - the close-out.
      - **`docs/architecture.md`:** project networks and the blue-green DNS behaviour, volumes, env versions, the redactor, engine checks; Background work unchanged.
      - The roadmap; plan 0002's M3b status.
      - The measurements: S2, full S4, S5, S6, S7 configured, S10, S11, the `deploys.log` peak.
      - The RC result.
      - The release note on service DNS after the next deploy.
    - Tests:
      - links resolve;
      - CI green;
      - full S4 green;
      - sentinel suite green.
    - Reviewers: `reviewer`, `security` (required), `systems`, `footprint`, `accessibility`.

## Test plan

- **Unit:** every test named in the steps.
- **Integration (stable; the RC where its job runs):**
  - volumes and stop-first;
  - bind mounts;
  - authenticated registry;
  - the candidate resolving peers during its health gate;
  - resolution within a project, and no reach across projects (both addresses);
  - OOM and cgroup limits;
  - `nat-unprotected`;
  - the M3a suite, including kill -9 at `start` with two networks;
  - goleak.
- **Sentinel suite (rule 7; plan 0004 hand-off).** The planted values are a plain env value, a secret env value and a registry password.
  - **The secret env value and the registry password are never in:**
    - the database bytes;
    - `deploys.log`, `deploys.get` or `deploys.list` responses;
    - deploy-log files on disk;
    - `jobs.payload_json` or `jobs.error`;
    - audit rows from `services.create`, `env_set` and the jobs;
    - server, Caddy or agent logs;
    - OpenAPI examples.
  - **The plain env value** is never in the database bytes or the audit rows. It may appear in `env_get` responses, in the container, and **in deploy logs**: it is not a secret, so it is not redacted.
- **Route matrix:**
  - every new or changed action;
  - `Sensitive` actions deny tokens and MCP;
  - admin actions deny `read` and `write` tokens;
  - the rollback env rule;
  - `AuditRead` rows;
  - `reconciler.remove_network` is 404 for every credential.
- **Fuzz:** the five fuzzers plus the decoders for the new ops. Seeds run in CI; each gets a 5-minute local run.
- **Playwright + axe** in all 8 projects. **A manual screen-reader pass for steps 13–14**, by the maintainer.
- **Bench:** see the footprint section.
- **Manual 1 GB VPS check (informational).** If a 1 GB host is available:
  - deploy a two-service project: an app that connects to `redis:6379` at start, and Redis;
  - check that the app passes its health gate on the first deploy;
  - give the app a secret, a named volume and a 256 MB limit;
  - redeploy under a `curl` loop: only maintenance 503s during the gap;
  - trigger an OOM;
  - pull a private image;
  - check the Docker card;
  - record idle VmRSS in research 05.

## Reviews required

- **`reviewer`:** the plan and every step.
- **`security`** (required): the plan; steps 1–15.
- **`systems`:**
  - the plan, including the B1 sign-off;
  - steps 1, 2, 3, 6, 7, 8, 9 (required), 10, 11 and 15.
- **`footprint`:**
  - steps 1, 2 and 3;
  - step 4 (full S4);
  - step 7 (the `deploys.log` peak);
  - steps 10 and 11 (S2 checks);
  - steps 12 and 15.
- **`accessibility`** (required on UI steps): steps 13, 14 and 15.

## Risks

| # | Risk | Likelihood | Mitigation |
|---|---|---|---|
| R1 | A secret reaches a log through a path or form the redactor does not see | medium | forms, streaming, fail closed, sentinel; accepted for transformed or generated values |
| R2 | Bind mounts give host access to a mistaken or stolen admin session | medium | off by default, `Sensitive`, real denied paths with ancestors, overlap refusal, re-resolve, owner and mode rules |
| R3 | A blue-green candidate gets peer traffic before its gate passes (B1) | medium for internal APIs | stated; stop-first never overlaps; the candidate is removed within `health_timeout_s` |
| R4 | The request line limits large environments | medium | a 24 KiB cap, measured exactly and tested at the worst case |
| R5 | A rollback restores an env that points at a retired database | medium | dialog warnings; the `write`-token rule |
| R6 | Forced stop-first adds an outage | certain, by design | maintenance page; read-only volumes keep blue-green |
| R7 | `10.250.0.0/16` collides with a VPN or LAN | low | per-project refusal; `--project-pool` |
| R8 | The RC fails because of a Docker bug | medium | not required; note at release |
| R9 | An OOM during an events gap is missed | low | stated |
| R10 | S4 creeps | low | QUICK S4, full S4 after step 4 and at step 15 |
| R11 | A registry lookup is slow or fails | low | 5 s bound; transient retry |
| R12 | An admin reads secrets by swapping the image | certain for a hostile admin | accepted; QM3b-11 |

## Traceability

### Roadmap items → steps

| Item | M3b part | Steps |
|---|---|---|
| 8 Env vars & secrets | versions, reveal, redaction, sentinel | 1, 5, 7, 13 |
| 12 Registries; volumes; project network with DNS | all | 2, 3, 4, 6, 8, 9, 14 |
| 14 CPU/RAM limits | limits, OOM | 4, 10, 14 |
| 15 Context variables | four `SD_` vars | 4, 7 |
| 17 RC in CI; version warning | RC, change, pre-28, `nat-unprotected` | 2, 11, 12, 14 |
| 23 Accessible UI | new screens | 13, 14 |

### Plan 0002 M3b section → where

| Requirement | Where |
|---|---|
| Snapshot references the env version | snapshot v2 `env_version_id`, frozen at enqueue; steps 1, 7 |
| Reveal `Sensitive`, intent first, nothing revealed on failure | `env_reveal`; step 5 |
| Credentials only per pull | Registry credentials; steps 2, 3, 6 |
| Alias = service name | Per-project network (at create, B1); step 9 |
| Bind mounts admin-only | Bind mounts; steps 4, 8 |
| Context variables | steps 4, 7 |
| Engine version in `meta`; change → needs attention | step 11 |
| Footprint, no new timers | Footprint budget |
| AAD; sentinel; fuzzed paths | Data model; Test plan; step 4 |
| Tests: `registry:2` auth; DNS; stop-first by volume; OOM; RC; Playwright | steps 6, 9, 8, 10, 12, 13–14 |
| Exit: RC matrix green (**amended by QM3b-6**); sentinel; bench | steps 12, 15 |
| "M3b adds known secret-store values" | the redactor (per deploy, on `redact.New`) |
| Rule 10 for registry hosts | `CheckRegistryAddrs`; dial part in M5 |

### Plan 0004 hand-offs → where

| Hand-off | Where |
|---|---|
| `deploys.log` redactor and sentinel | the redactor; step 7 |
| Audit or sensitive `deploys.log` | `AuditRead`; QM3b-5 |
| Registry rule-10 checks | step 6 |
| `no-new-privileges` per service | step 10 |
| Automatic stop-first for writable volumes | steps 7, 8 |
| CPU, memory and disk limits | step 10; QM3b-2 |
| Engine warning; `nat-unprotected` | steps 2, 11 |
| Cold pull and leak loop | S10, S11; step 15 |
| Stateful candidate's 10 s grace | the Storage screen |

### Notes this plan hands to later plans

- **0006 (M4a):** set `SD_FQDN`. Domains are service settings, read at run time.
- **0007 (M5):** set `SD_COMMIT`. `netpolicy` may reuse the address classes.
- **0008 (M6):** Compose maps onto env versions, `service_volumes` (the bind rules apply) and project networks. Volumes are not backed up.
- **0009 (M7a):**
  - `doctor --bundle` excludes `deploy-logs/` and never holds decrypted env;
  - `doctor` checks project subnets and the app network's gateway mode;
  - the install docs cover `--bind-root` (`nosuid,nodev`) and the pre-28 warning.
- **0010 (M7b):** backups hold app-secret ciphertexts and the key; a restore with a wrong key goes through `--forget-app-secrets`.
- **0011 (M8):** cleanup lists detached and unattached volumes; OOM may feed notifications.
- **0012 (M9):** MCP never gets `env_get`, `env_reveal` or `deploys.log`.

## Questions for the maintainer

**Maintainer answers (2026-11-28):** the maintainer accepted every recommended answer (QM3b-1 to QM3b-9 and QM3b-11) and the R3 trade-off (a blue-green candidate may answer its service name before its health gate passes). QM3b-10 is a note.

Each question has a recommended answer, and the design assumes it.

1. **QM3b-1 — May `write` tokens change env?**
   **Recommended: no (`admin`).** Env decides where an app's secrets go.
2. **QM3b-2 — Per-container disk limits?**
   **Recommended: none in M3b**, stated honestly: Docker can't enforce them on overlay2 over ext4. M8 adds disk alerts.
3. **QM3b-3 — A default memory limit?**
   **Recommended: none.** Show a hint, and report OOMs.
4. **QM3b-4 — Bind mounts off by default, enabled with `--bind-root`, and fresh 2FA for storage changes?**
   **Recommended: yes.**
5. **QM3b-5 — Audit every `deploys.log` read (not `Sensitive`), with fail-closed redaction?**
   **Recommended: yes.**
6. **QM3b-6 — Make the RC CI entry not required?**
   At release it must be green, or carry a Docker-bug note that the maintainer checks. This amends plan 0002's "RC matrix green" exit item.
   **Recommended: yes.**
7. **QM3b-7 — Does a rollback restore the target deploy's env version?**
   It would come with dialog warnings, and a `write` token would need `admin` to change the env this way.
   **Recommended: yes.**
8. **QM3b-8 — Apply rule-10 address checks to every registry host?**
   LAN registries would then need "allow private address".
   **Recommended: yes.**
9. **QM3b-9 — Docker before 28: warn only?**
   **Recommended: yes.**
10. **QM3b-10 (note only):** the S4 planning number of 46.7 MB follows plan 0002's re-base rule. The plan 0002 table is updated in the reorder PR.
11. **QM3b-11 — Make `services.set_source`, and image changes through `services.update`, `Sensitive` for services that have a bind mount?**
    Such services reach host files, and swapping the image is the easiest way to read every secret.
    **Recommended: yes.** It costs one step-up for a rare action.
    Mechanism: `Sensitive` is a fixed per-action flag, so this is an inside-the-action check: for a service with a bind mount, `set_source`, an image change through `update`, and `services.rollback` to a deploy from another repository require fresh 2FA and deny every token, audited as a denial (reviewer and security round 2).
