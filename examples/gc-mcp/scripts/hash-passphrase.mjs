// Prints an scrypt1$... hash for GC_MCP_OWNER_HASH. Reads the passphrase from
// the terminal WITHOUT echo (or from stdin when piped). Never prints it.
import readline from "node:readline";
import crypto from "node:crypto";
const rl = readline.createInterface({ input: process.stdin, output: process.stderr, terminal: process.stdin.isTTY });
let muted = false;
rl._writeToOutput = (s) => { if (!muted) process.stderr.write(s); };
rl.question("Owner passphrase (min 12 chars): ", (pass) => {
  muted = false; process.stderr.write("\n");
  if (pass.length < 12) { console.error("too short"); process.exit(1); }
  const salt = crypto.randomBytes(16);
  const h = crypto.scryptSync(pass, salt, 32, { N: 16384, r: 8, p: 1 });
  console.log(`scrypt1$${salt.toString("base64url")}$${h.toString("base64url")}`);
  rl.close();
});
muted = true;
