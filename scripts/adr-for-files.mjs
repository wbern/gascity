#!/usr/bin/env node
// Resolve this repo's ADRs (doc/adr, applies_to frontmatter) for an exact set of changed paths.
// The review prepare step treats failure here as a hard stop.
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const adrDir = path.join(root, 'doc/adr')
const statuses = new Set(['accepted', 'proposed', 'superseded', 'deprecated', 'rejected'])

function unquote(value) {
  const text = value.trim()
  if ((text.startsWith("'") && text.endsWith("'")) ||
      (text.startsWith('"') && text.endsWith('"'))) return text.slice(1, -1)
  return text
}

function parseAdr(file) {
  const lines = fs.readFileSync(path.join(adrDir, file), 'utf8').split(/\r?\n/)
  if (lines[0] !== '---') throw new Error(`${file}: missing frontmatter`)
  const end = lines.indexOf('---', 1)
  if (end < 0) throw new Error(`${file}: unclosed frontmatter`)
  let status = ''
  let appliesTo = null
  for (let i = 1; i < end; i++) {
    const line = lines[i]
    const statusMatch = line.match(/^status:\s*(.+)$/)
    if (statusMatch) status = unquote(statusMatch[1])
    if (/^applies_to:\s*$/.test(line)) {
      appliesTo = []
      while (i + 1 < end && /^\s+-\s+/.test(lines[i + 1])) {
        const pattern = unquote(lines[++i].replace(/^\s+-\s+/, ''))
        if (!pattern) throw new Error(`${file}: empty applies_to pattern`)
        appliesTo.push(pattern)
      }
    }
  }
  if (!statuses.has(status)) throw new Error(`${file}: invalid status`)
  if (!appliesTo?.length || !appliesTo.some((p) => !p.startsWith('!')))
    throw new Error(`${file}: missing applies_to patterns`)
  return {
    id: file.slice(0, 4), status, appliesTo,
    title: lines.slice(end + 1).find((line) => /^#\s+/.test(line))?.replace(/^#\s+/, '') ?? file,
  }
}

function globSource(glob) {
  let source = ''
  for (let i = 0; i < glob.length;) {
    const char = glob[i]
    if (char === '*' && glob[i + 1] === '*') {
      if (glob[i + 2] === '/') { source += '(?:.*/)?'; i += 3 }
      else { source += '.*'; i += 2 }
    } else if (char === '*') { source += '[^/]*'; i++ }
    else if (char === '?') { source += '[^/]'; i++ }
    else if (char === '{') {
      const end = glob.indexOf('}', i + 1)
      if (end < 0) throw new Error(`unclosed glob alternation: ${glob}`)
      source += `(?:${glob.slice(i + 1, end).split(',').map(globSource).join('|')})`
      i = end + 1
    } else { source += char.replace(/[.+^${}()|[\]\\]/g, '\\$&'); i++ }
  }
  return source
}

function matches(file, patterns) {
  const match = (pattern) => new RegExp(`^${globSource(pattern)}$`).test(file)
  return patterns.some((p) => !p.startsWith('!') && match(p)) &&
    !patterns.some((p) => p.startsWith('!') && match(p.slice(1)))
}

function main(files) {
  if (!files.length) throw new Error('at least one changed path is required')
  const entries = fs.readdirSync(adrDir).filter((f) => /^\d{4}-.*\.md$/.test(f)).sort()
  if (!entries.length) throw new Error('doc/adr has no ADRs')
  const applicable = entries.map(parseAdr).map((adr) => ({
    ...adr, files: files.filter((file) => matches(file, adr.appliesTo)),
  })).filter((adr) => adr.files.length)
  const accepted = applicable.filter((adr) => adr.status === 'accepted')
  const proposed = applicable.filter((adr) => adr.status === 'proposed')
  if (!accepted.length && !proposed.length) {
    console.log('No applicable ADRs for the given files.')
    return
  }
  for (const [label, adrs] of [['Accepted (enforced)', accepted], ['Proposed (advisory)', proposed]]) {
    if (!adrs.length) continue
    console.log(`${label}:`)
    for (const adr of adrs) {
      console.log(`  ADR-${adr.id} — ${adr.title}`)
      for (const file of adr.files) console.log(`    matched: ${file}`)
    }
    console.log('')
  }
}

try { main(process.argv.slice(2)) }
catch (error) {
  console.error(`adr-for-files: ${error.message}`)
  process.exitCode = 1
}
