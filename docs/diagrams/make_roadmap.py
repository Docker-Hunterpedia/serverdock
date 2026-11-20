import json, sys, random, html, os

out_dir = sys.argv[1]
os.makedirs(out_dir, exist_ok=True)

phases = [
 ("Phase 0 · Foundations", "IN PROGRESS", "#ffd8a8", "#fff3bf", "#f59e0b", 0, 110, 440,
  "[x] Research: 10+ tools, final gap check\n[x] Vision, features, architecture docs\n[x] Roadmap + threat model draft\n[x] ADRs 0001-0006 accepted\n    (AGPL, Go, outbound agent, Caddy,\n     SvelteKit, MCP-first AI)\n[x] 13 wireframe screens\n[x] Agent team (10 agents)\n[ ] Repo skeleton + CI\n[ ] Footprint bench on a 1 GB VM\n[ ] Spike 1: Go + Caddy + SQLite binary\n[ ] Spike 2: outbound agent + logs\n[ ] Threat model review\n\nExit: <= 80 MB idle, measured"),
 ("Phase 1 · MVP: Solo on one server", "Not started", "#a5d8ff", "#e7f5ff", "#4a9eed", 540, 110, 440,
  "- Install, setup token, admin + 2FA, onboarding\n- Action registry + audit log\n- Deploy: image, Dockerfile, Compose\n- GitHub push-to-deploy\n- Blue-green, health gate, auto-rollback\n- Stop-first for stateful apps\n- Domains, auto HTTPS, diagnostics, IPv6\n- Secrets, registry logins, volumes, network\n- Postgres/MySQL/Redis + S3 backups\n- Metrics, logs per deploy, alerts\n- Per-app CPU/RAM limits, disk cleanup\n- Self-update, update notices, uninstall\n- REST API + read-only MCP\n\nExit: first app on HTTPS < 5 min"),
 ("Phase 2 · Multi-server & builds", "Not started", "#d0bfff", "#f3f0ff", "#8b5cf6", 1080, 110, 440,
  "- Remote agents (NAT ok), builder servers\n- Railpack; GitLab, Gitea, Bitbucket\n- PR previews, environments, promote\n- Web terminal; Docker resources view\n- CI-tested templates + auto-update policies\n- Static sites, TCP/UDP, Cloudflare Tunnel\n- One-off run, release phase, cron\n- MongoDB, ClickHouse, DB upgrades\n- Minimal multi-user; serverdock.toml\n- Adopt existing stacks; move apps\n- CLI; MCP write actions + approval\n\nExit: 3 servers (1 behind NAT),\nremote agent <= 50 MB"),
 ("Phase 3 · Light built-in AI + ops", "Not started", "#eebefa", "#fff0f6", "#ec4899", 0, 730, 340,
  "- BYO Claude key, model per job\n- Auto explanations: alerts, failed deploys\n- \"Explain this\" button + in-panel chat\n- Vulnerability scan, security checks\n- Project export / import\n- Scale-to-zero, usage & cost view\n- Postgres PITR, DB password rotation\n\nExit: AI red-team review passed"),
 ("Phase 4 · Teams", "Not started", "#c3fae8", "#e6fcf5", "#06b6d4", 540, 730, 340,
  "- Orgs, projects, roles (free, no paywall)\n- OIDC / SAML SSO, audit export\n- GitOps state file + drift detection\n- External secret managers\n- Second-person approval for prod deploys\n- Terraform / Pulumi provider\n\nExit: RBAC test covers 100% of routes"),
 ("Phase 5 · Company & scale", "Not started", "#b2f2bb", "#ebfbee", "#22c55e", 1080, 730, 340,
  "- Optional Postgres / HA control plane\n- WireGuard mesh, registry-less push\n- Proxy security: rate limits, SSO\n- Importers, Hetzner provisioning\n- One app on many servers + load balancing\n- Status pages, plugins, procedures\n- Rootless/Podman, GPU, air-gapped\n- Desktop control-plane mode"),
]
TITLE = "ServerDock Roadmap: Phases 0-5"
LEGEND = "[x] done   [ ] to do   ·   Amber = in progress   ·   others = not started   ·   updated 2026-11-20"
W = 500

# ---------- .excalidraw ----------
els = []
def base(t, i, x, y, w, h, **k):
    d = {"type": t, "id": i, "x": x, "y": y, "width": w, "height": h, "angle": 0,
         "strokeColor": "#1e1e1e", "backgroundColor": "transparent", "fillStyle": "solid",
         "strokeWidth": 2, "strokeStyle": "solid", "roughness": 1, "opacity": 100,
         "groupIds": [], "frameId": None, "roundness": None, "seed": random.randint(1, 2**31),
         "version": 1, "versionNonce": random.randint(1, 2**31), "isDeleted": False,
         "boundElements": [], "updated": 1, "link": None, "locked": False}
    d.update(k); return d
def text(i, x, y, s, size, container=None, align="left", valign="top", color="#1e1e1e"):
    lines = s.split("\n")
    w = max(len(l) for l in lines) * size * 0.55
    h = len(lines) * size * 1.25
    return base("text", i, x, y, w, h, strokeColor=color, text=s, originalText=s, fontSize=size,
                fontFamily=1, textAlign=align, verticalAlign=valign, containerId=container,
                lineHeight=1.25, autoResize=True)

els.append(text("title", 330, 0, TITLE, 36))
els.append(text("legend", 330, 52, LEGEND, 18, color="#555555"))
for n, (name, status, hf, lf, st, x, y, lh, body) in enumerate(phases):
    hid, tid = f"h{n}", f"ht{n}"
    r = base("rectangle", hid, x, y, W, 80, backgroundColor=hf, strokeColor=st, roundness={"type": 3},
             boundElements=[{"type": "text", "id": tid}])
    els.append(r)
    label = f"{name}\n{status}"
    t = text(tid, x + 10, y + 12, label, 22, container=hid, align="center", valign="middle")
    t["width"] = W - 20
    els.append(t)
    els.append(base("rectangle", f"l{n}", x, y + 100, W, lh, backgroundColor=lf, strokeColor=st,
                    strokeWidth=1, roundness={"type": 3}))
    els.append(text(f"t{n}", x + 20, y + 118, body, 18))
def arrow(i, x, y, pts):
    xs = [p[0] for p in pts]; ys = [p[1] for p in pts]
    return base("arrow", i, x, y, max(xs) - min(xs), max(ys) - min(ys), points=pts,
                startArrowhead=None, endArrowhead="arrow", lastCommittedPoint=None,
                startBinding=None, endBinding=None)
els += [arrow("a01", 500, 150, [[0, 0], [40, 0]]), arrow("a12", 1040, 150, [[0, 0], [40, 0]]),
        arrow("a23", 1330, 650, [[0, 0], [0, 40], [-1080, 40], [-1080, 80]]),
        arrow("a34", 500, 770, [[0, 0], [40, 0]]), arrow("a45", 1040, 770, [[0, 0], [40, 0]])]
doc = {"type": "excalidraw", "version": 2, "source": "https://excalidraw.com", "elements": els,
       "appState": {"viewBackgroundColor": "#ffffff", "gridSize": None}, "files": {}}
with open(os.path.join(out_dir, "roadmap-phases.excalidraw"), "w") as f:
    json.dump(doc, f, indent=1, ensure_ascii=False)

# ---------- SVG ----------
pad = 30
VW, VH = 1580 + pad * 2, 1170 + pad * 2
e = html.escape
s = [f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="{-pad} {-pad} {VW} {VH}" width="{VW}" height="{VH}" font-family="Segoe UI, Helvetica, Arial, sans-serif">',
     f'<rect x="{-pad}" y="{-pad}" width="{VW}" height="{VH}" fill="#ffffff"/>',
     '<defs><marker id="ah" markerWidth="10" markerHeight="10" refX="9" refY="5" orient="auto"><path d="M0,0 L10,5 L0,10 z" fill="#1e1e1e"/></marker></defs>',
     f'<text x="790" y="34" font-size="36" font-weight="700" text-anchor="middle" fill="#1e1e1e">{e(TITLE)}</text>',
     f'<text x="790" y="74" font-size="18" text-anchor="middle" fill="#555555">{e(LEGEND)}</text>']
for name, status, hf, lf, st, x, y, lh, body in phases:
    s.append(f'<rect x="{x}" y="{y}" width="{W}" height="80" rx="14" fill="{hf}" stroke="{st}" stroke-width="2"/>')
    s.append(f'<text x="{x+W/2}" y="{y+35}" font-size="22" font-weight="600" text-anchor="middle" fill="#1e1e1e">{e(name)}</text>')
    s.append(f'<text x="{x+W/2}" y="{y+63}" font-size="18" text-anchor="middle" fill="#1e1e1e">{e(status)}</text>')
    s.append(f'<rect x="{x}" y="{y+100}" width="{W}" height="{lh}" rx="14" fill="{lf}" stroke="{st}" stroke-width="1"/>')
    for k, line in enumerate(body.split("\n")):
        weight = ' font-weight="600"' if line.startswith("Exit") else ""
        s.append(f'<text x="{x+20}" y="{y+140+k*25}" font-size="18" fill="#1e1e1e" xml:space="preserve"{weight}>{e(line)}</text>')
for x1, y1, x2, y2 in [(500, 150, 538, 150), (1040, 150, 1078, 150), (500, 770, 538, 770), (1040, 770, 1078, 770)]:
    s.append(f'<line x1="{x1}" y1="{y1}" x2="{x2}" y2="{y2}" stroke="#1e1e1e" stroke-width="2" marker-end="url(#ah)"/>')
s.append('<polyline points="1330,650 1330,690 250,690 250,728" fill="none" stroke="#1e1e1e" stroke-width="2" marker-end="url(#ah)"/>')
s.append('</svg>')
with open(os.path.join(out_dir, "roadmap-phases.svg"), "w") as f:
    f.write("\n".join(s))
print("ok", len(els))
