import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, mkdirSync, copyFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const resolver = path.join(root, 'scripts/adr-for-files.mjs')
const run = (script, ...files) => spawnSync(process.execPath, [script, ...files], {
  encoding: 'utf8', timeout: 10000,
})

test('this repo binds accepted ADR-0001 to its hook paths', () => {
  const result = run(resolver, '.githooks/pre-push', 'doc/adr/README.md')
  assert.equal(result.status, 0, result.stderr)
  assert.match(result.stdout, /Accepted \(enforced\):[\s\S]*ADR-0001/)
  assert.match(result.stdout, /matched: \.githooks\/pre-push/)
})

test('every ADR in doc/adr parses (status + applies_to)', () => {
  const result = run(resolver, 'no/such/path.go')
  assert.equal(result.status, 0, result.stderr)
})

test('a malformed ADR corpus refuses a clean answer', (t) => {
  const fixture = mkdtempSync(path.join(tmpdir(), 'gascity-adr-resolver-'))
  t.after(() => rmSync(fixture, { recursive: true, force: true }))
  mkdirSync(path.join(fixture, 'scripts'))
  mkdirSync(path.join(fixture, 'doc/adr'), { recursive: true })
  copyFileSync(resolver, path.join(fixture, 'scripts/adr-for-files.mjs'))
  writeFileSync(path.join(fixture, 'doc/adr/0001-broken.md'), '---\nstatus: accepted\n---\n# Broken\n')
  const result = run(path.join(fixture, 'scripts/adr-for-files.mjs'), 'ansible/main.yml')
  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /applies_to/)
})

test('positive, negative and proposed scopes stay distinct', (t) => {
  const fixture = mkdtempSync(path.join(tmpdir(), 'infra-adr-scope-'))
  t.after(() => rmSync(fixture, { recursive: true, force: true }))
  mkdirSync(path.join(fixture, 'scripts'))
  mkdirSync(path.join(fixture, 'doc/adr'), { recursive: true })
  copyFileSync(resolver, path.join(fixture, 'scripts/adr-for-files.mjs'))
  writeFileSync(path.join(fixture, 'doc/adr/0001-private.md'),
    "---\nstatus: accepted\napplies_to:\n  - 'ansible/**'\n  - '!ansible/secrets/**'\n---\n# 0001. Private\n")
  writeFileSync(path.join(fixture, 'doc/adr/0002-proposal.md'),
    "---\nstatus: proposed\napplies_to:\n  - 'Makefile'\n---\n# 0002. Proposal\n")
  const result = run(path.join(fixture, 'scripts/adr-for-files.mjs'),
    'ansible/playbooks/install.yml', 'ansible/secrets/key.yml', 'Makefile')
  assert.equal(result.status, 0, result.stderr)
  assert.match(result.stdout, /Accepted \(enforced\):[\s\S]*ADR-0001/)
  assert.doesNotMatch(result.stdout, /matched: ansible\/secrets\/key\.yml/)
  assert.match(result.stdout, /Proposed \(advisory\):[\s\S]*ADR-0002/)
})
