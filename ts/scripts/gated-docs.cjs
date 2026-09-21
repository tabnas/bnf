
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
  "go/README.md",
  "rs/README.md"
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


// A declared page that is not on disk THROWS.
//
// This filtered instead, and the comment here claimed the filter made a
// renamed page "fail as a missing gate". It did the opposite: the page
// left the list, both halves of the gate carried on over what remained,
// and the coverage test passed because it only counts what the list
// returned. Deleting a page was the one way to stop it being checked.
function gatedDocs() {
  return present(PAGES, 'gated')
}


function tutorials() {
  return present(TUTORIALS, 'a tutorial')
}


function present(declared, what) {
  const gone = declared.filter((f) => !exists(f))
  if (0 < gone.length) {
    throw new Error(
      `gated-docs: declared ${what} but not on disk: ` + gone.join(', ') +
      '. Rename it here, or delete the entry deliberately.')
  }
  return declared
}


module.exports = { gatedDocs, tutorials, PAGES, WITHHELD }

if (require.main === module) {
  process.stdout.write(gatedDocs().join('\n') + '\n')
}
