// Drives the full OAuth 2.1 flow against a running gc-mcp, then calls a tool.
// Usage: DEMO_PASSPHRASE=... node scripts/oauth-demo.mjs http://127.0.0.1:47811
// Prints steps and statuses only; never prints tokens, codes or the passphrase.
import crypto from "node:crypto";
const base = process.argv[2], pass = process.env.DEMO_PASSPHRASE;
const b64u = (b) => b.toString("base64url");
const step = (s, v = "") => console.log(`- ${s}${v ? ": " + v : ""}`);
const jget = async (p) => (await fetch(base + p)).json();
const pr = await jget("/.well-known/oauth-protected-resource/mcp");
step("protected-resource metadata (RFC 9728)", `resource=${pr.resource}`);
const as = await jget("/.well-known/oauth-authorization-server");
step("AS metadata (RFC 8414)", `pkce=${as.code_challenge_methods_supported}, dcr=${!!as.registration_endpoint}`);
const unauth = await fetch(base + "/mcp", { method: "POST", headers: { "content-type": "application/json", accept: "application/json, text/event-stream" }, body: "{}" });
step("no token", `HTTP ${unauth.status}, WWW-Authenticate has resource_metadata=${/resource_metadata=/.test(unauth.headers.get("www-authenticate") ?? "")}`);
const reg = await (await fetch(base + "/register", { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ redirect_uris: ["https://claude.ai/api/mcp/auth_callback"], client_name: "demo", token_endpoint_auth_method: "none", grant_types: ["authorization_code", "refresh_token"], response_types: ["code"] }) })).json();
step("dynamic client registration (RFC 7591)", "ok");
const verifier = b64u(crypto.randomBytes(32)), challenge = b64u(crypto.createHash("sha256").update(verifier).digest());
const u = new URL(base + "/authorize");
Object.entries({ response_type: "code", client_id: reg.client_id, redirect_uri: "https://claude.ai/api/mcp/auth_callback", code_challenge: challenge, code_challenge_method: "S256", state: "demo", scope: "gc:read", resource: pr.resource }).forEach(([k, v]) => u.searchParams.set(k, v));
const page = await (await fetch(u)).text();
const rid = /name=rid value="([^"]+)"/.exec(page)[1];
step("consent page served", "asks for owner passphrase");
const bad = await fetch(base + "/consent", { method: "POST", redirect: "manual", headers: { "content-type": "application/x-www-form-urlencoded" }, body: new URLSearchParams({ rid, passphrase: "definitely wrong", decision: "approve" }) });
step("wrong passphrase", `HTTP ${bad.status}, redirect=${bad.headers.get("location") ? "yes (BAD)" : "none"}`);
const ok = await fetch(base + "/consent", { method: "POST", redirect: "manual", headers: { "content-type": "application/x-www-form-urlencoded" }, body: new URLSearchParams({ rid, passphrase: pass, decision: "approve" }) });
const code = new URL(ok.headers.get("location")).searchParams.get("code");
step("owner approves", `HTTP ${ok.status}, code issued`);
const tok = await (await fetch(base + "/token", { method: "POST", headers: { "content-type": "application/x-www-form-urlencoded" }, body: new URLSearchParams({ grant_type: "authorization_code", client_id: reg.client_id, code, code_verifier: verifier, redirect_uri: "https://claude.ai/api/mcp/auth_callback", resource: pr.resource }) })).json();
step("token exchange (PKCE S256, audience-bound)", `token_type=${tok.token_type}, expires_in=${tok.expires_in}, scope=${tok.scope}`);
const replay = await fetch(base + "/token", { method: "POST", headers: { "content-type": "application/x-www-form-urlencoded" }, body: new URLSearchParams({ grant_type: "authorization_code", client_id: reg.client_id, code, code_verifier: verifier, redirect_uri: "https://claude.ai/api/mcp/auth_callback", resource: pr.resource }) });
step("replay of the same code", `HTTP ${replay.status}`);
const call = await fetch(base + "/mcp", { method: "POST", headers: { "content-type": "application/json", accept: "application/json, text/event-stream", authorization: "Bearer " + tok.access_token }, body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/call", params: { name: "city_status", arguments: {} } }) });
const txt = await call.text();
const line = txt.split("\n").find((l) => l.startsWith("data:")) ?? txt;
step("authorized tools/call city_status", `HTTP ${call.status} -> ${JSON.parse(line.replace(/^data:\s*/, "")).result.content[0].text}`);
