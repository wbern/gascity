// Network exposure settings, resolved and validated in ONE place so the process can refuse to
// start in an unsafe shape. Fail closed: binding beyond loopback requires a pinned peer (the
// reverse proxy) AND a trusted-proxy hop count, so a LAN neighbour cannot reach the app or
// spoof a client address.
const V4 = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/;

function v4(ip: string): number[] | null {
  const m = V4.exec(ip);
  if (!m) return null;
  const o = m.slice(1).map(Number);
  return o.every((n) => n <= 255) ? o : null;
}

export function isPrivateOrLoopback(ip: string): boolean {
  if (ip === "::1") return true;
  const o = v4(ip);
  if (!o) return false;
  const [a, b] = o;
  if (a === 127) return true;
  if (a === 10) return true;
  if (a === 172 && b >= 16 && b <= 31) return true;
  if (a === 192 && b === 168) return true;
  if (a === 100 && b >= 64 && b <= 127) return true; // CGNAT / Tailscale
  return false;
}

export interface NetworkConfig { bindHost: string; peerAllow: string[] | undefined; trustProxy: number }

export function resolveNetwork(env: Record<string, string | undefined>): NetworkConfig {
  const bindHost = (env.GC_MCP_BIND_HOST ?? "127.0.0.1").trim();
  if (!isPrivateOrLoopback(bindHost)) {
    throw new Error(`GC_MCP_BIND_HOST must be a loopback or private IP address (never 0.0.0.0, :: or a public/DNS name), got "${bindHost}"`);
  }
  const hops = (env.GC_MCP_TRUST_PROXY ?? "0").trim();
  if (!/^[0-3]$/.test(hops)) throw new Error(`GC_MCP_TRUST_PROXY must be an integer from 0 to 3, got "${hops}"`);
  const trustProxy = Number(hops);

  const raw = (env.GC_MCP_PEER_ALLOW ?? "").split(",").map((s) => s.trim()).filter(Boolean);
  for (const p of raw) {
    if (!isPrivateOrLoopback(p) || p.includes("/")) throw new Error(`GC_MCP_PEER_ALLOW entries must be exact private IP addresses, got "${p}"`);
  }
  const peerAllow = raw.length ? raw : undefined;

  const loopback = bindHost === "127.0.0.1" || bindHost === "::1";
  if (!loopback) {
    if (!peerAllow) throw new Error("binding beyond loopback requires GC_MCP_PEER_ALLOW (the reverse proxy's exact address)");
    if (trustProxy < 1) throw new Error("binding beyond loopback requires GC_MCP_TRUST_PROXY >= 1 (otherwise every caller looks like the proxy)");
  }
  return { bindHost, peerAllow, trustProxy };
}
