import { describe, it, expect } from "vitest";
import { parseCidr, inAnyCidr } from "../src/cidr.js";
describe("cidr", () => {
  const c = ["160.79.104.0/21", "153.61.208.0/20"].map(parseCidr);
  it("matches inside, rejects outside, handles mapped v6", () => {
    expect(inAnyCidr("160.79.104.1", c)).toBe(true);
    expect(inAnyCidr("160.79.111.255", c)).toBe(true);
    expect(inAnyCidr("160.79.112.0", c)).toBe(false);
    expect(inAnyCidr("::ffff:153.61.215.9", c)).toBe(true);
    expect(inAnyCidr("8.8.8.8", c)).toBe(false);
  });
  it("never matches garbage or undefined", () => {
    expect(inAnyCidr(undefined, c)).toBe(false);
    expect(inAnyCidr("not-an-ip", c)).toBe(false);
    expect(inAnyCidr("999.1.1.1", c)).toBe(false);
  });
  it("rejects a bad CIDR", () => {
    expect(() => parseCidr("1.2.3.4/40")).toThrow();
    expect(() => parseCidr("nope")).toThrow();
  });
});
