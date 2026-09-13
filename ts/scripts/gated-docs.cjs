
const Fs = require('node:fs')
const Path = require('node:path')

const REPO = Path.join(__dirname, '..', '..')

// The reader-facing set. Working documents (design notes, feasibility
// reports, ledgers) are deliberately out: see "The published set" in
// docs/STYLE-GUIDE.md.
const PAGES = [
  "go/doc/tutorial.md",
  "go/doc/guide.md",
  "go/doc/reference.md",
  "go/doc/concepts.md",
  "go/doc/differences.md",
  "README.md",
  "ts/README.md",
  "go/README.md"
]

const TUTORIALS = [
  "go/doc/tutorial.md"
]

// Pages that exist but are not this repository's documentation, so a
// passing prose gate over them would certify the wrong content. Each
// entry names why.
//
// EMPTY, and kept so. The four Go pages that were here held tabnas/zon's
// documentation, copied in and never rewritten; they are now this
// package's own and are gated above. The map stays because the failure
// it guards against is a copy nobody noticed, and the next one will
// arrive the same way.
const WITHHELD = {}


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
