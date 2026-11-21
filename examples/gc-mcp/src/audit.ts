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

// Append-only JSONL. Arguments are redacted and clipped; tokens are never passed in.
export class AuditLog {
  constructor(private file: string | null, private now: () => Date = () => new Date()) {
    if (file) fs.mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 });
  }
  write(e: AuditEvent) {
    const line = JSON.stringify({ ts: this.now().toISOString(), ...e, args: e.args === undefined ? undefined : redactText(JSON.stringify(e.args)).slice(0, 300), note: e.note ? redactText(e.note).slice(0, 200) : undefined });
    if (this.file) fs.appendFileSync(this.file, line + "\n", { mode: 0o600 });
  }
}
