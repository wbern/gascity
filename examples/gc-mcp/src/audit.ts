import fs from "node:fs";
import path from "node:path";
import { redactText } from "./redact.js";

export interface AuditEvent {
  kind: "tool" | "auth";
  client?: string;
  tool?: string;
  args?: unknown;
  ok?: boolean;
  ms?: number;
  chars?: number;
  note?: string;
}

// Append-only JSONL, size-rotated (keeps .1 .2 .3). Arguments and notes are redacted and
// clipped; tokens and passphrases are never passed in.
export class AuditLog {
  constructor(private file: string | null, private now: () => Date = () => new Date(), private maxBytes = 5_000_000) {
    if (file) fs.mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 });
  }
  private rotate() {
    if (!this.file) return;
    try {
      if (fs.statSync(this.file).size < this.maxBytes) return;
    } catch { return; }
    for (let i = 2; i >= 1; i--) { try { fs.renameSync(`${this.file}.${i}`, `${this.file}.${i + 1}`); } catch { /* missing */ } }
    fs.renameSync(this.file, `${this.file}.1`);
  }
  write(e: AuditEvent) {
    if (!this.file) return;
    this.rotate();
    const line = JSON.stringify({ ts: this.now().toISOString(), ...e,
      args: e.args === undefined ? undefined : redactText(JSON.stringify(e.args)).slice(0, 300),
      note: e.note ? redactText(e.note).slice(0, 200) : undefined });
    fs.appendFileSync(this.file, line + "\n", { mode: 0o600 });
  }
}
