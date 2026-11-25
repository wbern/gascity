// Tiny IPv4 CIDR matcher (IPv4-mapped IPv6 accepted). Anything unparseable is NOT a match.
function toInt(ip: string): number | null {
  const m = /^(?:::ffff:)?(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/i.exec(ip.trim());
  if (!m) return null;
  const o = m.slice(1).map(Number);
  if (o.some((n) => n > 255)) return null;
  return ((o[0] << 24) | (o[1] << 16) | (o[2] << 8) | o[3]) >>> 0;
}
export function parseCidr(c: string): { net: number; mask: number } {
  const [ip, bitsS] = c.split("/");
  const net = toInt(ip), bits = Number(bitsS);
  if (net === null || !Number.isInteger(bits) || bits < 0 || bits > 32) throw new Error(`bad CIDR: ${c}`);
  const mask = bits === 0 ? 0 : (0xffffffff << (32 - bits)) >>> 0;
  return { net: (net & mask) >>> 0, mask };
}
export function inAnyCidr(ip: string | undefined, cidrs: { net: number; mask: number }[]): boolean {
  if (!ip) return false;
  const n = toInt(ip);
  return n !== null && cidrs.some((c) => ((n & c.mask) >>> 0) === c.net);
}
