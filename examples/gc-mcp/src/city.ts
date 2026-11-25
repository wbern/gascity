// Which city this server fronts: never a hard-coded name. GC_MCP_CITY if set; otherwise asked from the supervisor (GET /v0/cities), and only when
// there is exactly one. With several cities the operator must say which: guessing would read and message the wrong one.
export async function resolveCity(baseUrl: string, envCity?: string, fetchImpl: typeof fetch = fetch): Promise<string> {
  const set = (envCity ?? "").trim();
  if (set) return set;
  let names: string[] = [];
  try {
    const r = await fetchImpl(`${baseUrl.replace(/\/$/, "")}/v0/cities`, { headers: { accept: "application/json" }, redirect: "error", signal: AbortSignal.timeout(5000) });
    if (!r.ok) throw new Error(`HTTP ${r.status}`);
    names = (((await r.json()) as { items?: { name?: string }[] }).items ?? []).map((c) => String(c.name ?? "")).filter(Boolean);
  } catch (e) { throw new Error(`GC_MCP_CITY is not set and the supervisor's city list could not be read (${(e as Error).message}); set GC_MCP_CITY`); }
  if (names.length === 1) return names[0];
  throw new Error(names.length ? `GC_MCP_CITY is not set and the supervisor runs ${names.length} cities (${names.join(", ")}); set GC_MCP_CITY to one of them` : "GC_MCP_CITY is not set and the supervisor lists no cities");
}
