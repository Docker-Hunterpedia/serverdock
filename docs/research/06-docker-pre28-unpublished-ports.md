# Research: Docker Engine before 28.0 and LAN access to unpublished container ports (N13)

*Researched 2026-11-27 by an AI research agent for plan 0004 step 15 (accepted risk N13). Not yet confirmed by the maintainer. All sources were fetched on 2026-11-27. Quotes are exact, but some long sentences are given only in part (marked "..."), because the fetch tool returns excerpts. Nothing here was tested on a real host.*

## Short answer

- **Affected:** Docker Engine on Linux with iptables, **every version before 28.0.0** (28.0.0 came out 2025-02-19). Docker does not give a first affected version. The behaviour comes from how bridge networking worked from the start, so it covers the whole range ServerDock supports before 28: **25.x, 26.x and 27.x (the last 27.x is 27.5.1, 2025-01-22)**.
- **Not a CVE:** no CVE or moby security advisory covers unpublished ports. Docker calls the 28.0 change "security hardening". The only CVE close to this, CVE-2025-54388 / GHSA-x4rx-4gw3-53p4, is about *published* ports after a firewalld reload on 28.2.0–28.3.2, and it says unpublished ports stayed protected.
- **No backport:** no 27.x release note mentions the fix, and the fixing PRs have milestone 28.0.0.
- **Conditions:** all of these must hold. (a) IPv4 forwarding is on. (b) The host's `filter` `FORWARD` chain policy is **ACCEPT**. (c) An attacker on the same link-layer network, or on a network routed to the host, sends packets for the container's subnet to the host (for example `ip route add <bridge subnet> via <host IP>`) and knows or guesses the container's IP. Since Docker 1.13, Docker sets the FORWARD policy to DROP when *it* turns forwarding on, so a default install is usually **not** exposed. Hosts where something else turned forwarding on before Docker started (VPN, router, Kubernetes, LXD, an earlier sysctl) keep ACCEPT and **are** exposed. The mechanism covers any bridge network in `nat` mode, not just the default `docker0` (see §2).
- **28.0 default:** per-port filtering in the `DOCKER` chain no longer depends on the FORWARD policy. Unpublished ports can't be reached by direct routing from other hosts, but the Docker host itself can still reach them. A new `raw`-table rule also drops direct remote connections to container IPs in `nat` mode, even to published ports. The default gateway mode stays `nat`.
- **Opt-outs that bring the old behaviour back:** `com.docker.network.bridge.gateway_mode_ipv4=nat-unprotected` (per network, 28.0.0). Two others loosen only part of it: `allow-direct-routing` (daemon option, **28.2.0**) and `com.docker.network.bridge.trusted_host_interfaces` (per network, 28.2.0) open direct routing to *published* ports only. `DOCKER_INSECURE_NO_IPTABLES_RAW=1` (environment variable for the daemon, 28.0.2) skips the raw-table rules. `ip-forward-no-drop` keeps FORWARD at ACCEPT but, Docker says, "still drops traffic for unpublished ports using separate rules".

## Evidence

### 1. Version range and fix

**Docker Engine 28 release notes**, 28.0.0 (2025-02-19), https://docs.docker.com/engine/release-notes/28/:
- "Direct routed access to container ports that are not exposed using `p`/`-publish` is now blocked in the `DOCKER` iptables chain." ([moby/moby#48724](https://github.com/moby/moby/pull/48724))
- "If the default iptables filter-FORWARD policy was previously left at `ACCEPT` on your host, and direct routed access to a container's unpublished ports from a remote host is still required, options are: ..."
- "Unpublished container ports continue to be directly accessible from the Docker host via the container's IP address."
- "Fix a security issue that was allowing remote hosts to connect directly to a container on its published ports." ([moby/moby#49325](https://github.com/moby/moby/pull/49325))
- "Fix a security issue that was allowing neighbor hosts to connect to ports mapped on a loopback address." (#49325)

**Docker blog**, "Docker Engine v28: Hardening Container Networking by Default", Tim Welsh, posted 2025-02-28, https://www.docker.com/blog/docker-engine-28-hardening-container-networking-by-default/:
- "This behavior only affects Linux users running Docker versions earlier than 28.0.0 with `iptables`. Docker Desktop is **not** affected."
- "This update isn't about fixing a single vulnerability — it's about security hardening so your containers stay safe."

**moby/moby#48724**, "Remove dependency on the filter-FORWARD policy", merged 2024-10-23, milestone 28.0.0, https://github.com/moby/moby/pull/48724:
- "A remote host with a route to the container's network, via the docker host, could access any port."
- The page shows no backport to 27.x.

**moby/moby#49325**, "libnet/d/bridge: drop connections to lo mappings, and direct remote connections", merged 2025-01-28, milestone 28.0.0, closes #45610, https://github.com/moby/moby/pull/49325:
- The PR "fixes two separate security issues that have a common cause -- lack of proper packet filtering before NATing."
- It "adds a new rule in the raw-PREROUTING chain to filter ingress connections targeting the container's IP address" (applies when the gateway mode is `nat`).
- The PR doesn't mention a backport. A third-party commit linked on the page says "no 27.x backport". That isn't an authoritative source, but it agrees with the release notes below.

**Docker Engine 27 release notes**, https://docs.docker.com/engine/release-notes/27/: no 27.x entry mentions unpublished ports, the filter-FORWARD policy, the raw table, `nat-unprotected`, #48724 or #49325. The last 27.x release listed is 27.5.1 (2025-01-22). In 27.0.1, `gateway_mode_ipv4=<nat|routed>` was added, without `nat-unprotected`.

**Port publishing docs**, https://docs.docker.com/engine/network/port-publishing/: "In releases older than 28.0.0, hosts within the same L2 segment (for example, ..." This note is about ports published to a *loopback* address and links to [moby/moby#45610](https://github.com/moby/moby/issues/45610).

**Security advisories** (all three pages of https://github.com/moby/moby/security/advisories, checked 2026-11-27): none is about unpublished ports or direct routing to container IPs. The closest is GHSA-x4rx-4gw3-53p4 / CVE-2025-54388, https://github.com/moby/moby/security/advisories/GHSA-x4rx-4gw3-53p4: affected "28.2.x, 28.3.0, 28.3.1, 28.3.2", patched ">= 28.3.3", "Unpublished ports remain inaccessible." It only affects 28.x, so it doesn't belong in N13. It is noted here so nobody mistakes it for N13.

**Start of the range:** no primary source gives a first affected version. moby/moby#28257 (milestone 1.13.0, merged 2016-11-10, https://github.com/moby/moby/pull/28257) says: "With this change if `ip_forward` is enabled by docker daemon then the iptables FORWARD policy will be set to DROP." and "On a docker upgrade if `ip_forward` is already enabled this change will not take effect." So from 1.13 to 27.x, whether a host was exposed depended on the FORWARD policy. Before 1.13, Docker didn't set a policy at all. *Unverified:* the exact behaviour before 1.13. It doesn't matter for ServerDock, which requires Engine 25+.

### 2. Conditions

Blog (link above):
- "However, if your host's `filter-FORWARD` chain is permissive (i.e., `ACCEPT` by default) **and** `net.ipv4.ip_forward` is enabled, unpublished ports could also be remotely accessible under certain conditions."
- "This only affects hosts on the **same physical/link-layer network** as the Docker host. In multi-tenant LAN environments or other shared local networks, someone connected on an RFC1918 subnet (such as `192.168.x.x` or `10.x.x.x`) could reach unpublished container ports if they knew (or guessed) its IP address."
- Example route: `ip route add 172.17.0.0/16 via 192.168.0.10`. "From there, if `192.168.0.10` is your Docker host with a permissive firewall, the attacker could send packets straight to `172.17.0.x` (the container's IP)."
- The three conditions: "`net.ipv4.ip_forward` was enabled (often auto-enabled by Docker if it wasn't already)." / "The system-wide `FORWARD` chain was set to `ACCEPT`." / "Another machine on the same LAN (or an attached subnet) routed traffic to the container's IP address."

moby/moby#45610 (opened 2023-05-25, reproduced on Engine 24.0.1, milestone 28.0.0), https://github.com/moby/moby/issues/45610: a container with no published port is not reachable "unless `FORWARD` chain is set to `ACCEPT`". It reproduces access to the container IP by adding a route to `172.17.0.0/16` through the Docker host.

What these sources mean for each part of the question:
- **Same L2 segment plus a route:** yes. The attacker must be able to get packets addressed to the container subnet to the host, which in practice means being on the same link (or a network the host routes for) and adding a route. Docker's blog also says "(or an attached subnet)", so "same L2 segment" is the common case rather than a strict limit.
- **IP forwarding:** required. On a host running Docker it is always on.
- **Host firewall:** this is the deciding factor. FORWARD policy ACCEPT means exposed (before 28). DROP means only published ports are reachable. A host firewall that drops forwarded traffic from the external interface to the bridge subnet also blocks it.
- **Default vs user-defined bridge:** the blog's example uses the default bridge ("containers on the default bridge network"), but #48724 talks about "the container's network" in general, and the 28.0 fix applies to all bridge networks in `nat` mode. *Inference, not stated directly by Docker:* a user-defined bridge such as ServerDock's `10.250.x.0/24` network is affected the same way. `enable_icc=false` and the `DOCKER-ISOLATION` chains only control traffic between containers and between bridge networks, not traffic coming in from outside. No source I found links `DOCKER-ISOLATION` to this issue.
- **IPv4 vs IPv6:** the blog and #48724 describe IPv4. The 28.0.0 notes also change IPv6: "Docker will now only set the `ip6tables` policy for the `FORWARD` chain in the `filter` table to `DROP` if it enables IP forwarding on the host itself" (#48594), and the port-publishing docs say that now "the Docker daemon blocks access to ports that have not been published" for both. *Unverified:* exactly how IPv6 unpublished ports behaved before 28. Not relevant to ServerDock M3a, whose app network is IPv4 only.
- **Cloud VPS:** *unverified.* Many cloud networks don't deliver packets addressed to IPs a VM doesn't own, which would make this hard to exploit on a typical VPS. I found no primary source to rely on, so the docs shouldn't claim it.

### 3. What 28.0 does by default, and the opt-outs

- **Default gateway mode is still `nat`.** Bridge driver docs, https://docs.docker.com/engine/network/drivers/bridge/: the `gateway_mode_ipv4` row reads "Control external connectivity." with default `nat`. Port publishing docs: "The default is `nat`, NAT and masquerading rules are set up for each published container port."
- **Default protection.** Port publishing docs: "By default, remote hosts are not allowed direct access to container IP addresses in Docker's Linux bridge networks." "They can only access ports published to host IP addresses." Blog: "In Docker 28.0, we now explicitly drop unsolicited inbound traffic to each container's internal IP unless that port was explicitly published (`-p` or `--publish`). This doesn't affect local connections from the Docker host itself, but it does block remote LAN connections to unpublished ports."
- **`nat-unprotected`** (brings the old behaviour back, per network). 28.0.0 notes: "Bridge driver options `com.docker.network.bridge.gateway_mode_ipv4` and `com.docker.network.bridge.gateway_mode_ipv6` now accept mode `nat-unprotected`." "`nat-unprotected` is similar to the default `nat` mode, but no per port/protocol rules are set up." "This means any port on a container can be accessed by direct-routing from a remote host." Port publishing docs: "In `nat-unprotected` mode, unpublished container ports are also accessible using direct routing," and Docker describes it as being there for compatibility with the old default behaviour.
- **`routed`:** no NAT. Firewall rules still allow only published ports, which remote hosts reach directly at the container IP.
- **`allow-direct-routing`** (daemon option, real name confirmed, added in **28.2.0**, not 28.0). 28.2.0 notes: "Add daemon option `"allow-direct-routing"` to disable filtering of packets from outside the host ..." (#49832). Port publishing docs: "To allow direct access to any published port, on any container, in any Linux bridge network, ..." **It affects published ports only.** Unpublished ports stay blocked; for those you need `nat-unprotected`.
- **`com.docker.network.bridge.trusted_host_interfaces`** (28.2.0, #49832): "These interfaces have direct access to published ports on container IP addresses."
- **`DOCKER_INSECURE_NO_IPTABLES_RAW=1`** (28.0.2, #49621): "When enabled, Docker will not create rules in the iptables `raw` table." "This option bypasses some of the security hardening introduced in Docker Engine 28.0.0." *Unverified:* whether this alone brings back access to unpublished ports. The release note says it "reduces security by allowing other hosts ...", but the `DOCKER`-chain filtering from #48724 is separate.
- **`ip-forward-no-drop`** (blog): it "preserves a globally open `FORWARD` chain. But keep in mind that Docker still drops traffic for unpublished ports using separate rules."
- **`--iptables=false`** (any version): the firewall page, https://docs.docker.com/engine/network/packet-filtering-firewalls/, says that without Docker's rules "all of their ports will be accessible to hosts on the local network."
- **Later releases:** 28.0.1 moved most rules out of `filter-FORWARD` into Docker's own chains (#49518). 28.2.2 stopped adding an explicit `RETURN` to `DOCKER-USER` (#50098).

### 4. Mitigations on engines before 28

1. **Upgrade to Engine 28.0.0 or later** (preferably ≥ 28.3.3, because of CVE-2025-54388). This is the only fix Docker offers. The blog gives no other advice for older engines.
2. **Make sure the FORWARD policy is DROP** (`iptables -L FORWARD -n`; the blog's checklist suggests checking `iptables -L -n` and `ip6tables -L -n`). With DROP, Docker before 28 only allows published ports (#45610). Watch out: this can break other forwarding on the host, such as a VPN or routing, and the policy must be made to persist across reboots.
3. **A `DOCKER-USER` rule.** Docker's iptables docs, https://docs.docker.com/engine/network/firewall-iptables/, describe `DOCKER-USER` as "A placeholder for user-defined rules that will be processed before rules in the `DOCKER-FORWARD` and `DOCKER` chains" and give source-based examples such as `iptables -I DOCKER-USER -i ext_if ! -s 192.0.2.0/24 -j DROP`. For ServerDock's app subnet, one rule could drop forwarded traffic that arrives on the external interface for that subnet and was not DNAT'd. *My own suggestion, not from Docker's docs and not tested:* `iptables -I DOCKER-USER -i <ext_if> -d <app subnet> -m conntrack ! --ctstate DNAT -j DROP`. Caddy runs on the host, so its connections to apps leave through OUTPUT, not FORWARD, and this rule would not affect them. (Before 28.0.1 the `DOCKER-USER` chain name was the same. Docker only creates it when its iptables management is on.)
4. **Don't share an untrusted L2 segment** with the host (shared office or campus LANs, some hosting "private networks"), or put the host behind a firewall that drops traffic addressed to Docker subnets.

## Recommendation for ServerDock

**Proposed accepted-risk text for `docs/security.md` (N13):**

> **Accepted risk N13: older Docker engines can let nearby machines reach app ports directly.** On Docker Engine versions before 28.0.0 (ServerDock accepts 25 to 27), a machine on the same local network as the host can sometimes connect to an app container's ports directly, even though ServerDock never publishes them. It does this by sending traffic for ServerDock's app subnet to the host. This works only when the host's firewall forwards traffic by default (the iptables `FORWARD` policy is `ACCEPT`), which is common when something other than Docker turned on IP forwarding, for example a VPN or router setup. Docker Engine 28.0.0 and later block this by default. To fix it, upgrade Docker (28.3.3 or later is best). If you can't, set the `FORWARD` policy to `DROP` or add a `DOCKER-USER` rule that drops outside traffic to the app subnet, and don't share the host's local network with machines you don't trust.

**What ServerDock could do, without overclaiming:**
- **Warn at start when the engine is older than 28.0.0.** ServerDock already negotiates the API version (Engine 25+), so the engine version is known. The plan currently puts "the Docker version-change warning" in M3b (plan line 114). A pre-28 warning isn't in plan 0004, so adding one would need a plan change. The warning should say "may be reachable", because ServerDock can't easily see the FORWARD policy or the host's LAN without root and iptables access, and it shouldn't pretend to have checked.
- **Keep the default gateway mode.** `network_ensure` should never set `gateway_mode_ipv4` (so it stays `nat`), and it could refuse to adopt or reuse a network whose options say `nat-unprotected`, so ServerDock never brings the old exposure back on 28+.
- **Don't claim "container IPs are reachable from the host only".** Even on 28+, an operator can turn the protection off (`nat-unprotected`, `--iptables=false`, `DOCKER_INSECURE_NO_IPTABLES_RAW`). The wording should be "on Docker 28+ with default settings".

**Trade-offs.** Making Engine 28 the minimum would remove the risk, but it would rule out distributions that still ship 27.x, and Docker's own 28.0 networking changes broke some setups (see the 28.0.1/28.0.2 fixes). A warning plus documentation keeps the setup simple and the footprint unchanged. Writing firewall rules for the user would be more intrusive: it touches host firewall state, could conflict with ufw or firewalld, and goes beyond ServerDock's "safe operations" promise.

## Not verified

- The first engine version affected (no primary source; the behaviour depends on the FORWARD policy, and Docker has set it only when Docker enables forwarding itself since 1.13).
- How IPv6 unpublished ports behaved before 28.
- Whether typical cloud VPS networks make this impossible in practice.
- Whether `DOCKER_INSECURE_NO_IPTABLES_RAW=1` on its own makes unpublished ports reachable again.
- The `DOCKER-USER` rule in §4.3 has not been tested.
