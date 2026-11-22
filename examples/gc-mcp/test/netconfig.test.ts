import { describe, it, expect } from "vitest";
import { resolveNetwork, isPrivateOrLoopback, resolveMcpPath } from "../src/netconfig.js";

describe("isPrivateOrLoopback", () => {
  it("accepts loopback, RFC1918 and tailnet/CGNAT addresses", () => {
    for (const ip of ["127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "172.31.255.254", "192.168.23.69", "100.64.0.1", "100.127.255.1"]) {
      expect(isPrivateOrLoopback(ip), ip).toBe(true);
    }
  });
  it("rejects public, wildcard, link-local-ish and malformed values", () => {
    for (const ip of ["0.0.0.0", "::", "8.8.8.8", "79.136.99.79", "172.32.0.1", "100.128.0.1", "192.169.0.1", "169.254.1.1", "gc.example.se", "", "999.1.1.1", "192.168.1", "1.2.3.4.5"]) {
      expect(isPrivateOrLoopback(ip), ip).toBe(false);
    }
  });
});

describe("resolveNetwork", () => {
  it("defaults to loopback with no peer restriction and no trusted proxy", () => {
    expect(resolveNetwork({})).toEqual({ bindHost: "127.0.0.1", peerAllow: undefined, trustProxy: 0 });
  });
  it("refuses a public or wildcard bind address", () => {
    for (const h of ["0.0.0.0", "::", "79.136.99.79", "example.se"]) {
      expect(() => resolveNetwork({ GC_MCP_BIND_HOST: h, GC_MCP_PEER_ALLOW: "192.168.23.5", GC_MCP_TRUST_PROXY: "1" }), h).toThrow(/private|loopback/i);
    }
  });
  it("refuses a non-loopback bind unless the peer is pinned AND a proxy is trusted", () => {
    expect(() => resolveNetwork({ GC_MCP_BIND_HOST: "192.168.23.69" })).toThrow(/GC_MCP_PEER_ALLOW/);
    expect(() => resolveNetwork({ GC_MCP_BIND_HOST: "192.168.23.69", GC_MCP_PEER_ALLOW: "192.168.23.5" })).toThrow(/GC_MCP_TRUST_PROXY/);
  });
  it("accepts a pinned LAN bind and normalises the peer list", () => {
    expect(resolveNetwork({ GC_MCP_BIND_HOST: "192.168.23.69", GC_MCP_PEER_ALLOW: " 192.168.23.5 , 192.168.23.6", GC_MCP_TRUST_PROXY: "1" }))
      .toEqual({ bindHost: "192.168.23.69", peerAllow: ["192.168.23.5", "192.168.23.6"], trustProxy: 1 });
  });
  it("refuses a peer list entry that is not an exact private address", () => {
    for (const bad of ["8.8.8.8", "192.168.23.0/24", "*", "nginx.lan"]) {
      expect(() => resolveNetwork({ GC_MCP_PEER_ALLOW: bad }), bad).toThrow(/GC_MCP_PEER_ALLOW/);
    }
  });
  it("rejects a malformed trust-proxy hop count", () => {
    for (const bad of ["-1", "abc", "1.5", "9"]) expect(() => resolveNetwork({ GC_MCP_TRUST_PROXY: bad }), bad).toThrow(/GC_MCP_TRUST_PROXY/);
  });
});

describe("resolveMcpPath", () => {
  it("defaults to /mcp and accepts a prefix, a nested prefix, or the root", () => {
    expect(resolveMcpPath({})).toBe("/mcp");
    expect(resolveMcpPath({ GC_MCP_PATH: "/mcp/gc" })).toBe("/mcp/gc");
    expect(resolveMcpPath({ GC_MCP_PATH: "/" })).toBe("/");
  });
  it("refuses anything that could shadow an OAuth endpoint, add a query, or break routing", () => {
    for (const bad of ["mcp", "/mcp/", "//x", "/a b", "/a?x=1", "/a#b", "/..", "/a/../b", "/authorize", "/token", "/register", "/revoke", "/consent", "/healthz", "/.well-known", "/.well-known/x", "/mcp\n"]) {
      expect(() => resolveMcpPath({ GC_MCP_PATH: bad }), JSON.stringify(bad)).toThrow(/GC_MCP_PATH/);
    }
  });
});
