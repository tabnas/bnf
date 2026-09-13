
const Fs = require('node:fs')
const Path = require('node:path')

const REPO = Path.join(__dirname, '..', '..')

// The reader-facing set. Working documents (design notes, feasibility
// reports, ledgers) are deliberately out: see "The published set" in
// docs/STYLE-GUIDE.md.
const PAGES = [
  "go/doc/differences.md",
  "README.md",
  "ts/README.md",
  "go/README.md"
]

const TUTORIALS = []

// Pages that exist but are not this repository's documentation, so a
// passing prose gate over them would certify the wrong content. Each
// entry names why. Tracked in tabnas/bnf#52; the fix is to write these
// pages, not to gate the copies.
const WITHHELD = {
  "go/doc/concepts.md": "byte-identical to tabnas/zon's; documents the ZON plugin",
  "go/doc/guide.md": "byte-identical to tabnas/zon's; documents the ZON plugin",
  "go/doc/reference.md": "byte-identical to tabnas/zon's; documents the ZON plugin",
  "go/doc/tutorial.md": "byte-identical to tabnas/zon's; documents the ZON plugin"
}


function exists(rel) {
  return Fs.existsSync(Path.join(REPO, rel))
}


// Filtered to what is actually on disk, so a renamed page fails as a
// missing gate rather than as a crash.
function gatedDocs() {
  return PAGES.filter(exists)
}


function tutorials() {
  return TUTORIALS.filter(exists)
}


module.exports = { gatedDocs, tutorials, PAGES, WITHHELD }

if (require.main === module) {
  process.stdout.write(gatedDocs().join('\n') + '\n')
}
