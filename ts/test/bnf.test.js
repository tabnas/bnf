/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */

// Smoke tests for the notation-neutral compiler. The heavy verification
// lives downstream: @tabnas/abnf's suite drives this pipeline through
// RFC 3986, left recursion, round-trips and a 68-file conformance corpus.
// What is checked here is that the package's own surface works without a
// front-end present.

const { describe, it } = require('node:test')
const assert = require('node:assert')

const {
  emitGrammarSpec,
  eliminateLeftRecursion,
  refsIn,
  escapeRegExp,
  BUILTIN_TOKENS,
  toJsonic,
  compileSpec,
  attachActions,
  markListing,
  toPureSpec,
  toRecognitionSpec,
  EmitError,
  VERSION,
} = require('../dist/bnf')

const ref = (name) => ({ kind: 'ref', name })
const term = (literal) => ({ kind: 'term', literal })
const token = (name) => ({ kind: 'token', name })

describe('bnf', () => {
  it('compiles a minimal IR grammar into a GrammarSpec', () => {
    const spec = emitGrammarSpec({
      productions: [
        { name: 'val', alts: [[ref('add')]] },
        { name: 'add', alts: [[token('#NR')]] },
      ],
    }, { tag: 'demo' })

    assert.ok(spec.rule.val, 'expected a val rule')
    assert.ok(spec.rule.add, 'expected an add rule')
  })

  it('stamps the caller-supplied group tag, not a notation name', () => {
    const spec = emitGrammarSpec({
      productions: [{ name: 'top', alts: [[term('x')]] }],
    }, { tag: 'demo' })

    const tags = JSON.stringify(spec)
    assert.match(tags, /demo/)
    assert.doesNotMatch(tags, /\babnf\b/, 'must not assume a notation')
  })

  it('defaults the tag to bnf', () => {
    // /bnf/ was the whole assertion here, and 'abnf' contains it: the
    // default could have been a notation name and this test would still
    // have passed. Measured — setting the default to 'abnf' left all 56
    // tests green. Go's default WAS 'abnf', and its twin test was blind
    // the same way, so the divergence survived on both sides at once.
    // Read the group tags instead, and require the head of each to be
    // this package's own name.
    const spec = emitGrammarSpec({
      productions: [{ name: 'top', alts: [[term('x')]] }],
    })
    const tags = [...JSON.stringify(spec).matchAll(/"g":"([^",]*)/g)]
      .map((m) => m[1])
    assert.notEqual(tags.length, 0, 'no group tags, so this proves nothing')
    for (const tag of tags) {
      assert.equal(tag, 'bnf',
        'the default must be this package own name, never a notation')
    }
  })

  it('does not reuse an action ref across attachActions calls', () => {
    // Two calls must not collide. Resetting the counter to 0 each call
    // would make the second overwrite the first call's function and
    // leave the first alt pointing at the replacement. TypeScript scans
    // the ref map first (src/spec.ts) and is correct; nothing pinned it
    // until now. Go reset the counter, and downstream on abnf's
    // `op = "inc" / "dec"` the INC alt's action ran zero times and the
    // DEC alt's ran twice, silently.
    const spec = emitGrammarSpec({
      productions: [
        { name: 'op', alts: [[term('inc')], [term('dec')]] },
      ],
    }, { tag: 'demo', marks: true })

    const noop = () => undefined
    for (const key of ['@op:o:INC', '@op:o:DEC']) {
      attachActions(spec, { [key]: [noop] })
    }

    const refs = Object.keys(spec.ref ?? {}).filter((k) => k.includes('_user'))
    assert.equal(refs.length, 2,
      'two calls, each attaching to one alt, must leave two refs: ' +
      JSON.stringify(refs))

    // The refs existing is not enough: the alts must point at different
    // ones, or one alt still runs the other's action.
    const used = new Set()
    const rs = spec.rule.op
    for (const alt of [...(rs.open ?? []), ...(rs.close ?? [])]) {
      for (const name of [alt.a].flat(9).filter((x) => 'string' === typeof x)) {
        if (name.includes('_user')) {
          assert.equal(used.has(name), false, 'two alts share ' + name)
          used.add(name)
        }
      }
    }
    assert.equal(used.size, 2,
      'expected the two alts to carry 2 distinct user refs, got ' +
      JSON.stringify([...used]))
  })

  // OPEN DIVERGENCE — the twin of Go's
  // TestActionRefPrefixDivergesFromTypeScript (go/bnf_test.go). The
  // compiler-generated action refs are named `@bnf_a<n>` here and
  // `@abnf_a<n>` in Go, whichever tag the caller passes.
  //
  // Pinned on both sides so the record cannot outlive the divergence:
  // repairing either port turns that port's test red and forces the pair
  // to be revisited together. The repair belongs on the Go side (this
  // port's name is the correct one — it names no notation), and it has to
  // wait for abnf/go/compile_test.go:66 and :251, which assert the
  // literal "@abnf_a" and would go vacuous the moment it changes.
  it('names generated action refs @bnf_a<n> (Go says @abnf_a<n>)', () => {
    const spec = emitGrammarSpec({
      productions: [{ name: 'top', alts: [[term('x')]] }],
    }, { tag: 'demo' })
    const prefixes = [...JSON.stringify(spec).matchAll(/"@([a-z]+)_a[0-9]+"/g)]
      .map((m) => m[1])
    assert.notEqual(prefixes.length, 0,
      'no action refs emitted, so this proves nothing')
    for (const prefix of prefixes) {
      assert.equal(prefix, 'bnf',
        'if this port has been repaired, delete this test and its Go ' +
        'twin in go/bnf_test.go together')
    }
  })

  it('lifts a single-literal production into a named lexer token', () => {
    const spec = emitGrammarSpec({
      productions: [
        { name: 'top', alts: [[ref('PL')]] },
        { name: 'PL', alts: [[term('+')]] },
      ],
    }, { tag: 'demo' })
    assert.equal(spec.options?.fixed?.token?.['#PL'], '+')
  })

  it('eliminates left recursion into iterative form', () => {
    const grammar = {
      productions: [
        { name: 'expr', alts: [[ref('expr'), term('+'), ref('num')], [ref('num')]] },
        { name: 'num', alts: [[token('#NR')]] },
      ],
    }
    const out = eliminateLeftRecursion(grammar)
    const expr = out.productions.find((p) => p.name === 'expr')
    const stillLeftRec = expr.alts.some(
      (alt) => alt[0] && alt[0].kind === 'ref' && alt[0].name === 'expr')
    assert.equal(stillLeftRec, false, 'expr must no longer start with itself')
  })

  it('collects rule references from a sequence', () => {
    const out = new Set()
    refsIn([ref('a'), { kind: 'group', alts: [[ref('b')]] }, term('x')], out)
    assert.deepStrictEqual([...out].sort(), ['a', 'b'])
  })

  it('escapes regex metacharacters in literals', () => {
    assert.equal(new RegExp('^' + escapeRegExp('a.b')).test('a.b'), true)
    assert.equal(new RegExp('^' + escapeRegExp('a.b')).test('axb'), false)
  })

  it('maps bareword names to engine built-in token names', () => {
    // `ident = TX` lets a grammar reference the lexer's whole-word token
    // instead of re-deriving it character by character.
    assert.equal(BUILTIN_TOKENS.NR, '#NR')
    assert.equal(BUILTIN_TOKENS.TX, '#TX')
    assert.equal(BUILTIN_TOKENS.ST, '#ST')
    assert.equal(BUILTIN_TOKENS.VL, '#VL')
  })

  it('serialises a spec as jsonic text', () => {
    const spec = emitGrammarSpec({
      productions: [{ name: 'top', alts: [[term('x')]] }],
    }, { tag: 'demo', builtins: true })
    const text = toJsonic(spec)
    assert.equal(typeof text, 'string')
    assert.ok(0 < text.length)
  })

  it('guards a repetition helper with its FOLLOW set', () => {
    // A repetition helper terminates on an empty alternative, which
    // names no token. The engine only offers a matcher where the active
    // rule names it, so without a guard the token that follows the
    // repetition is never lexed and the parse dies at the loop exit.
    // See issue #3.
    //
    //   root = star D    where   star = *W
    //
    // The generated `_gen1_star_W` must name `#D` on its terminating
    // alternative, peeking and pushing straight back so nothing extra
    // is consumed.
    const spec = emitGrammarSpec({
      productions: [
        {
          name: 'root',
          alts: [[{ kind: 'star', inner: ref('W') }, ref('D')]],
        },
        { name: 'W', alts: [[{ kind: 'regex', pattern: '[ ]', flags: '' }]] },
        { name: 'D', alts: [[{ kind: 'regex', pattern: '[0-9]', flags: '' }]] },
      ],
    }, { tag: 'demo' })

    // The helper itself, not its `$alt0`/`$step1` chain rules.
    const starRule = Object.entries(spec.rule)
      .find(([name]) => /^_gen\d+_star_/.test(name) && !name.includes('$'))
    assert.ok(starRule, 'expected a generated star helper')

    const [, rs] = starRule
    const dToken = spec.rule.D.open[0].s
    const guards = rs.open.filter((alt) => alt.s === dToken)
    assert.equal(
      guards.length, 1,
      'the star helper must name D on its terminating alternative')
    assert.equal(
      guards[0].b, 1,
      'the FOLLOW guard must push its peeked token back')
    assert.ok(
      !guards[0].p && !guards[0].r,
      'the FOLLOW guard must not push or replace a rule')
    // The unguarded empty alternative stays last as the fallback.
    const last = rs.open[rs.open.length - 1]
    assert.equal(last.s, undefined, 'expected a bare fallback alternative')
  })

  it('carries FOLLOW through a nullable suffix', () => {
    // `root = star X Y` where X is nullable: what follows the star is
    // FIRST(X) *and* FIRST(Y), because X can vanish.
    const spec = emitGrammarSpec({
      productions: [
        {
          name: 'root',
          alts: [[{ kind: 'star', inner: ref('W') }, ref('X'), ref('Y')]],
        },
        { name: 'W', alts: [[{ kind: 'regex', pattern: '[ ]', flags: '' }]] },
        { name: 'X', alts: [[token('#NR')], []] },
        { name: 'Y', alts: [[token('#TX')]] },
      ],
    }, { tag: 'demo' })

    const [, rs] = Object.entries(spec.rule)
      .find(([name]) => /^_gen\d+_star_/.test(name) && !name.includes('$'))
    const guarded = new Set(rs.open.map((alt) => alt.s).filter(Boolean))
    assert.ok(guarded.has('#NR'), 'expected FIRST(X) in the guard')
    assert.ok(
      guarded.has('#TX'),
      'expected FIRST(Y) in the guard, since X is nullable')
  })

  it('groups a regex before anchoring it', () => {
    // `^` binds tighter than `|`, so `^a|bc` anchors only the first
    // branch and the second can match anywhere in the input, producing
    // a token from the wrong position. Issue #2 defect 2.
    const spec = emitGrammarSpec({
      productions: [
        { name: 'top', alts: [[{ kind: 'regex', pattern: 'a|bc', flags: '' }]] },
      ],
    }, { tag: 'demo' })

    const re = Object.values(spec.options.match.token)[0]
    assert.equal(re.source, '^(?:a|bc)')
    // The point of the grouping: the second branch must not match at a
    // non-zero offset.
    assert.equal(re.test('xbc'), false)
    assert.equal(re.test('bc'), true)
  })

  it('keeps grammar-level remove/clearAll across the internal clone', () => {
    // `emitGrammarSpec` clones the grammar and then reads these fields
    // off the clone, so a clone that copied only `productions` dropped
    // them silently. Issue #2 defect 3.
    const spec = emitGrammarSpec({
      productions: [
        { name: 'top', alts: [[token('#NR')]] },
        { name: 'gone', alts: [[token('#TX')]] },
      ],
      remove: ['gone'],
      clearAll: true,
    }, { tag: 'demo', start: 'top' })

    assert.equal(spec.rule.gone, undefined, 'expected `gone` to be removed')
    assert.equal(spec.clear, true, 'expected clearAll to set spec.clear')
  })

  it('does not overwrite a user rule named __start__', () => {
    // The IR reserves no names, so a grammar may legitimately contain a
    // production called `__start__`. Issue #2 defect 4.
    const spec = emitGrammarSpec({
      productions: [
        { name: '__start__', alts: [[token('#NR')]] },
      ],
    }, { tag: 'demo' })

    const start = spec.options.rule.start
    assert.notEqual(
      start, '__start__',
      'the wrapper must not take the user rule\'s name')
    // The user's rule survives, and the wrapper pushes it rather than
    // pushing itself.
    assert.deepEqual(spec.rule.__start__.open[0].s, '#NR')
    assert.equal(spec.rule[start].open[0].p, '__start__')
  })

  it('escapes every control character in strict jsonic output', () => {
    // Strict mode promises valid JSON; a raw tab or CR inside a string
    // makes JSON.parse reject it. Issue #2 defect 5.
    const text = toJsonic({ s: 'a\tb\rcd' }, { strict: true })
    assert.doesNotThrow(() => JSON.parse(text))
    assert.equal(JSON.parse(text).s, 'a\tb\rcd')
  })

  it('allocates fresh action refs on a second attachActions call', () => {
    // The counter used to reset per call, so the second call reused
    // `@bnf_user0` and clobbered the first. Issue #2 defect 6.
    const spec = emitGrammarSpec({
      productions: [
        { name: 'top', alts: [[term('x')], [term('y')]] },
      ],
    }, { tag: 'demo', marks: true })

    // markListing renders `<rule>  o:<mark>  …`; the action ref for an
    // alt is `@<rule>:<phase>:<mark>`.
    const marks = markListing(spec)
      .split('\n')
      .map((line) => /^(\S+)\s+([oc]):(\S+)/.exec(line))
      .filter(Boolean)
      .filter((m) => m[1] === 'top' && m[3] !== '_')
      .map((m) => `@${m[1]}:${m[2]}:${m[3]}`)
    assert.ok(2 <= marks.length, 'expected at least two marked alts')

    attachActions(spec, { [marks[0]]: () => 'first' })
    attachActions(spec, { [marks[1]]: () => 'second' })

    const refs = Object.entries(spec.ref)
      .filter(([k]) => k.startsWith('@bnf_user'))
    assert.equal(refs.length, 2, 'expected two distinct action refs')
    assert.equal(
      new Set(refs.map(([k]) => k)).size, 2, 'ref names must be distinct')
  })

  it('eliminates left recursion hidden behind nullable sugar', () => {
    // `A = ["x"] A "y"` is left-recursive whenever the optional takes
    // its empty branch, but elimination runs before desugar and so only
    // sees a leading `opt`, not a leading ref. Issue #2 defect 1.
    const grammar = {
      productions: [{
        name: 'A',
        alts: [
          [{ kind: 'opt', inner: term('x') }, ref('A'), term('y')],
          [term('z')],
        ],
      }],
    }

    const out = eliminateLeftRecursion(grammar)
    const a = out.productions.find((p) => p.name === 'A')
    // No surviving alternative may re-enter A at the same position:
    // every alt either starts with something that must consume input,
    // or does not reference A first.
    for (const alt of a.alts) {
      const leads = alt.length > 0 && alt[0].kind === 'ref' &&
        alt[0].name === 'A'
      assert.equal(leads, false, 'A still re-enters itself immediately')
      const hidden = alt.length > 1 && alt[0].kind === 'opt' &&
        alt[1].kind === 'ref' && alt[1].name === 'A'
      assert.equal(hidden, false, 'A still re-enters itself behind an opt')
    }
  })

  // Left-recursion elimination turns `A = ["x"] A "y" / "z"` into
  // `A = ( "x" A "y" | "z" ) "y"*`. That is a correct CFG and a broken
  // parser: the tail loop is greedy, so the inner A eats the `"y"` the
  // enclosing `"x" A "y"` still owes and the outer alternative starves.
  // No lookahead settles it — the repeated token and the follow token
  // are the same token, and the answer depends on enclosing stack
  // depth. Issue #6.
  describe('a contested left-recursion tail loop yields on suffix debt', () => {
    const hidden = (...alts) => ({ productions: [{ name: 'A', alts }] })
    const x = { kind: 'term', literal: 'x', caseSensitive: true }
    const y = { kind: 'term', literal: 'y', caseSensitive: true }
    const z = { kind: 'term', literal: 'z', caseSensitive: true }
    const w = { kind: 'term', literal: 'w', caseSensitive: true }
    const lp = { kind: 'term', literal: '(', caseSensitive: true }
    const rp = { kind: 'term', literal: ')', caseSensitive: true }

    // Every alt of every rule, flattened — the counter machinery is
    // spread across a dispatcher and its `$altN` chain rules.
    const alts = (spec) => Object.entries(spec.rule).flatMap(
      ([name, rs]) => ['open', 'close'].flatMap((ph) => {
        const f = rs[ph]
        const list = Array.isArray(f) ? f : (f && f.alts) || []
        return list.map((a) => ({ rule: name, ...a }))
      }))

    it('counts the debt on the push and guards the loop with it', () => {
      const spec = emitGrammarSpec(
        hidden([{ kind: 'opt', inner: x }, ref('A'), y], [z]), { tag: 'demo' })

      // The alternative that pushes the inner A increments the counter…
      const pushes = alts(spec).filter((a) => a.n)
      assert.equal(pushes.length, 1, JSON.stringify(pushes))
      const [counter] = Object.keys(pushes[0].n)
      assert.equal(pushes[0].n[counter], 1, 'the push must add one debt')
      assert.equal(pushes[0].p, 'A', 'the debt rides on the push of A')

      // …and the tail loop's continue alternative refuses to run while
      // any debt is outstanding.
      const guarded = alts(spec).filter((a) => a.c)
      assert.equal(guarded.length, 1, JSON.stringify(guarded))
      assert.match(guarded[0].rule, /_star_/, 'the guard belongs to the loop')
      assert.deepEqual(guarded[0].c, { ['n.' + counter]: 0 })
      assert.equal(
        guarded[0].p, guarded[0].rule, 'the guarded alt is the loop back-edge')

      // The loop's exits stay unguarded, so it yields rather than fails.
      const loop = spec.rule[guarded[0].rule].open
      assert.ok(
        loop.slice(1).every((a) => !a.c),
        'the exit peeks and the bare fallback must stay unconditional')
    })

    it('resets the counter across an alternative that re-anchors', () => {
      // `"(" A ")"` owes a `")"`, not a `"y"`, so the A it pushes must
      // start from a clean slate or `x(zy)y` cannot parse.
      const spec = emitGrammarSpec(
        hidden(
          [{ kind: 'opt', inner: x }, ref('A'), y],
          [lp, ref('A'), rp],
          [z]),
        { tag: 'demo' })

      const deltas = alts(spec).filter((a) => a.n).map((a) => Object.values(a.n)[0])
      assert.deepEqual(
        deltas.slice().sort(), [0, 1],
        'expected one increment and one barrier reset, got ' +
        JSON.stringify(alts(spec).filter((a) => a.n)))
    })

    it('leaves a loop the suffix cannot contest alone', () => {
      // The loop repeats `"w"`; the only suffix after a self-reference
      // is `")"`, which never competes with it. Emitting a guard here
      // would make `(z)w` stop parsing.
      const spec = emitGrammarSpec(
        hidden([ref('A'), w], [lp, ref('A'), rp], [z]), { tag: 'demo' })

      assert.deepEqual(
        alts(spec).filter((a) => a.n || a.c), [],
        'an uncontested loop must compile exactly as before')
    })

    it('leaves a nullable suffix alone', () => {
      // `A = ["x"] A ["y"] / "z"` commits the enclosing frame to
      // nothing, so the loop stays greedy — and already parses its
      // whole language.
      const spec = emitGrammarSpec(
        hidden(
          [{ kind: 'opt', inner: x }, ref('A'), { kind: 'opt', inner: y }],
          [z]),
        { tag: 'demo' })

      assert.deepEqual(alts(spec).filter((a) => a.n || a.c), [])
    })

    it('leaves plain direct left recursion alone', () => {
      const spec = emitGrammarSpec(hidden([ref('A'), y], [z]), { tag: 'demo' })
      assert.deepEqual(alts(spec).filter((a) => a.n || a.c), [])
    })

    it('emits a counter per contested rule', () => {
      const spec = emitGrammarSpec({
        productions: [
          {
            name: 'B',
            alts: [
              [{ kind: 'opt', inner: { kind: 'term', literal: 'p', caseSensitive: true } },
                ref('B'), { kind: 'term', literal: 'q', caseSensitive: true }],
              [ref('A')],
            ],
          },
          {
            name: 'A',
            alts: [[{ kind: 'opt', inner: x }, ref('A'), y], [z]],
          },
        ],
      }, { tag: 'demo' })

      const counters = new Set(
        alts(spec).filter((a) => a.c).map((a) => Object.keys(a.c)[0]))
      assert.equal(counters.size, 2, [...counters].join(' '))
    })

    it('guards only the branches the suffix competes for', () => {
      // The loop repeats `"y"` and `"w"`; the suffix owes a `"y"`. Blocking
      // the `"w"` branch too would reject `xzwy`, where the inner A has to
      // consume the `w` before yielding the `y`.
      const spec = emitGrammarSpec(
        hidden([ref('A'), y], [ref('A'), w], [x, ref('A'), y], [z]),
        { tag: 'demo' })

      const tokenOf = (lit) => Object.entries(spec.options.fixed.token)
        .find(([, v]) => v === lit)[0]
      const loop = Object.entries(spec.rule)
        .find(([n]) => /_star_/.test(n) && !n.includes('$'))[1]

      // Continue alternatives fan out to K-token prefixes, so group them
      // by the head token that decides which branch they are.
      const heads = (list) =>
        [...new Set(list.map((a) => String(a.s).split(' ')[0]))].sort()
      const cont = loop.open.filter((a) => a.p)
      assert.deepEqual(
        heads(cont.filter((a) => a.c)), [tokenOf('y')], JSON.stringify(loop.open))
      assert.deepEqual(
        heads(cont.filter((a) => !a.c)), [tokenOf('w')], JSON.stringify(loop.open))
    })

    it('sees a self-reference buried in a group', () => {
      // The detector runs before desugar, where the recursive call is
      // still inside an IR `group`. Reading only the top level of each
      // seed missed this shape entirely.
      const spec = emitGrammarSpec(
        hidden([ref('A'), y], [{ kind: 'group', alts: [[x, ref('A'), y], [z]] }]),
        { tag: 'demo' })
      assert.equal(alts(spec).filter((a) => a.c).length, 1)
    })

    it('reduces a rule name to a counter name Go agrees on', () => {
      // The Go port sanitises by rune; a non-`u` regex here would split an
      // astral name into two surrogate halves and mint a different name
      // for the same grammar.
      const counter = (name) => {
        const spec = emitGrammarSpec(
          { productions: [{ name, alts: [[{ kind: 'opt', inner: x }, ref(name), y], [z]] }] },
          { tag: 'demo' })
        return Object.keys(alts(spec).find((a) => a.c).c)[0]
      }
      assert.equal(counter('a-b'), 'n.debt_a_b')
      assert.equal(counter('\u{1F600}'), 'n.debt__')
    })

    it('keeps the guard expressible as pure data', () => {
      // Compilation mode drops every closure; a guard that needed one
      // would make these grammars uncompilable rather than merely
      // untree-built.
      const spec = emitGrammarSpec(
        hidden([{ kind: 'opt', inner: x }, ref('A'), y], [z]),
        { tag: 'demo', builtins: true })
      const text = compileSpec(spec, { strict: true })
      assert.doesNotThrow(() => JSON.parse(text))
      assert.match(text, /"n\.debt_A"/)
    })
  })

  it('never allocates a lifted literal an engine-owned token name', () => {
    // `NR = "NR"` wanted `#NR`, which the lexer's number matcher owns.
    // The engine rejects a fixed.token entry under a matcher-owned name
    // at configuration time, so this turned an ordinary grammar into a
    // hard failure before parsing began. Issue #2, footer note.
    const lift = (lit) => emitGrammarSpec({
      productions: [
        { name: 'top', alts: [[ref(lit)]] },
        {
          name: lit,
          alts: [[{ kind: 'term', literal: lit, caseSensitive: true }]],
        },
      ],
    }, { tag: 'demo' }).options.fixed.token

    for (const owned of ['NR', 'TX', 'ST', 'VL', 'ZZ', 'SP', 'LN', 'CM']) {
      const fixed = lift(owned)
      assert.ok(
        !Object.prototype.hasOwnProperty.call(fixed, '#' + owned),
        `#${owned} is engine-owned and must not be allocated to a literal`)
      // The literal still gets a token, just under a free name.
      assert.deepEqual(Object.values(fixed), [owned])
    }

    // A name the engine does not own is still used as-is.
    assert.deepEqual(lift('PL'), { '#PL': 'PL' })
  })

  // Left factoring rewrites a user rule's alternatives, so it must fire
  // only where the dispatcher genuinely cannot separate them: a
  // factored rule keeps ONE alternative, which merges the per-branch
  // collision marks that actions bind to. The prefix-span measurement
  // is what draws that line, and it runs on the raw IR — where the
  // sugar kinds are still present and easy to misread as unbounded.
  describe('left factoring is bounded by dispatch lookahead', () => {
    const rep = (min, max, inner) => ({ kind: 'rep', min, max, inner })
    const group = (...alts) => ({ kind: 'group', alts })
    const factored = (grammar, name) =>
      Object.keys(emitGrammarSpec(grammar, { tag: 'demo' }).rule)
        .some((rn) => rn.startsWith(name + '$fact'))

    const twoAlts = (prefix) => ({
      productions: [
        {
          name: 'g',
          alts: [
            [...prefix, ref('P')],
            [...prefix, ref('Q')],
          ],
        },
        { name: 'P', alts: [[term('p')]] },
        { name: 'Q', alts: [[term('q')]] },
      ],
    })

    it('leaves a short prefix of plain terminals to the dispatcher', () => {
      assert.equal(factored(twoAlts([term('a'), term('x')]), 'g'), false)
    })

    it('leaves a short prefix wrapped in a group', () => {
      // `("a") "x"` spans two tokens like the case above. Reading the
      // group as unbounded factored it, and the two branches then
      // shared one mark.
      assert.equal(
        factored(twoAlts([group([term('a')]), term('x')]), 'g'), false)
    })

    it('leaves a short prefix containing an optional', () => {
      assert.equal(
        factored(twoAlts([{ kind: 'opt', inner: term('-') }, term('1')]), 'g'),
        false)
    })

    it('leaves a bounded repetition that fits the lookahead', () => {
      assert.equal(factored(twoAlts([rep(2, 2, term('a'))]), 'g'), false)
    })

    it('factors a prefix longer than the lookahead', () => {
      const long = ['a', 'b', 'c', 'd', 'e'].map(term)
      assert.equal(factored(twoAlts(long), 'g'), true)
    })

    it('factors an unbounded repetition', () => {
      assert.equal(
        factored(twoAlts([{ kind: 'plus', inner: term('a') }]), 'g'), true)
    })

    it('keeps a mark per branch when it does not factor', () => {
      const listing = markListing(
        emitGrammarSpec(twoAlts([group([term('a')]), term('x')]),
          { tag: 'demo', marks: true }))
      // Two distinct open marks — one per original alternative.
      const marks = listing.split('\n')
        .filter((l) => l.includes('o:'))
        .map((l) => l.trim())
      assert.equal(marks.length, 2, listing)
      assert.notEqual(marks[0], marks[1], listing)
    })
  })


  // Coverage feeding the contest checks must agree with what a matcher
  // actually matches, or no guard is emitted where one is needed.
  it('counts both cases of a case-insensitive literal as covered', () => {
    // A bare literal is case-insensitive by default here, so `"G"` also
    // matches `g` and contests a lowercase class.
    const spec = emitGrammarSpec({
      productions: [
        {
          name: 'g',
          alts: [
            [term('G'), ref('L')],
            [ref('L')],
          ],
        },
        { name: 'L', alts: [[{ kind: 'regex', pattern: '[a-z]', flags: '' }]] },
      ],
    }, { tag: 'demo' })

    // The contest was detected, so the keyword entry carries lookahead
    // and outranks the bare class entry.
    const first = spec.rule.g.open[0]
    assert.ok(
      'string' === typeof first.s && 1 < first.s.split(' ').length,
      'expected a multi-token keyword guard first, got ' +
      JSON.stringify(spec.rule.g.open))
  })


  it('exports a semver-shaped VERSION matching package.json', () => {
    assert.match(VERSION, /^\d+\.\d+\.\d+/)
    assert.equal(VERSION, require('../package.json').version)
  })
})


// The alt `m` mark is compiler-internal: `attachActions`,
// `attachActionSlots` and `markListing` read it off the in-memory spec,
// but the engine's alt contract has no `m` and the grammar JSON Schema
// sets additionalProperties:false. An emitted `m` therefore does not just
// waste bytes -- it makes the grammar FAIL `tabnas validate`.
describe('marks do not reach the wire', () => {
  const marked = () => emitGrammarSpec({
    productions: [
      { name: 'op', alts: [[{ kind: 'term', literal: 'inc' }],
                           [{ kind: 'term', literal: 'dec' }]] },
    ],
  }, { tag: 'demo', marks: true, builtins: true })

  it('markListing still reads marks off the in-memory spec', () => {
    assert.match(markListing(marked()), /op\s+o:/)
  })

  it('toPureSpec and toRecognitionSpec both drop m', () => {
    for (const shape of [toPureSpec, toRecognitionSpec]) {
      const out = toJsonic(shape(marked()), { strict: true })
      assert.doesNotMatch(out, /"m"\s*:/, `${shape.name} emitted a mark`)
    }
  })

  it('shaping does not mutate the caller\'s spec', () => {
    const spec = marked()
    toPureSpec(spec)
    assert.match(markListing(spec), /op\s+o:/,
      'toPureSpec stripped marks from the in-memory spec, not just its output')
  })
})


// A tail repeat's separator is moved out of `alts` and stashed on
// `tailRepeat`, so token allocation — which walks `alts` — never saw it.
// A separator whose literal appears nowhere else in the grammar
// therefore got no token, the emitted separator alternate came out as
// `s: ''`, and the repeat could never match. Mirrored by
// go/bnf_test.go TestTailRepeatSeparatorGetsAToken.
describe('tail-repeat separator', () => {
  // `list = DIGIT [ "," list ]` with the comma used NOWHERE else is the
  // isolating case. Real grammars usually reuse the separator literal in
  // another rule and pick up that rule's token by accident, which is how
  // this survived.
  const listGrammar = () => ({
    productions: [
      { name: 'doc', alts: [[ref('list')]] },
      {
        name: 'list',
        alts: [[
          { kind: 'regex', pattern: '[0-9]', flags: '' },
          {
            kind: 'opt',
            inner: { kind: 'group', alts: [[term(','), ref('list')]] },
          },
        ]],
      },
    ],
  })

  it('allocates a token for the separator', () => {
    const spec = emitGrammarSpec(listGrammar(), { start: 'doc', tag: 'demo' })
    const close = spec.rule.list.close
    const alts = Array.isArray(close) ? close : (close && close.alts) || []
    assert.ok(0 < alts.length, 'list has no close alternates')
    assert.ok(alts[0].s,
      'separator alternate names no token — the comma was never allocated ' +
      'one, so the repeat can never match')
    assert.match(alts[0].s, /^#/)
  })
})


// `spec.meta.provenance` maps every rule the compiler MINTED back to the
// author-written production it came from. A compiled grammar carries an
// order of magnitude more rules than the author wrote, and all of them
// surface in rule stacks, hover and completion, so a tool that cannot
// resolve a generated name has nothing useful to show.
describe('provenance', () => {
  // `doc` repeats `item`. The star helper is generated FOR doc, and the
  // only rule name embedded in its own generated name is `item` — the
  // rule being repeated. Attributing it to `item` is the mistake the map
  // exists to prevent, and the mistake any name-parsing implementation
  // would make.
  const repeatGrammar = () => ({
    productions: [
      { name: 'doc', alts: [[ref('item'), { kind: 'star', inner: ref('item') }]] },
      { name: 'item', alts: [[term('a')], [term('b')]] },
    ],
  })

  it('attributes a helper to its ENCLOSING rule, not the repeated one', () => {
    const spec = emitGrammarSpec(repeatGrammar(), { start: 'doc', tag: 'demo' })
    const prov = spec.meta.provenance
    const star = Object.keys(spec.rule).find((n) => n.startsWith('_gen') &&
      n.includes('star_item'))
    assert.ok(star, 'expected a generated star helper named after item')
    assert.equal(prov[star], 'doc',
      `${star} belongs to doc, which repeats item — not to item itself`)
  })

  it('attributes every generated rule, and only to author-written rules', () => {
    const grammar = repeatGrammar()
    const authored = new Set(grammar.productions.map((p) => p.name))
    const spec = emitGrammarSpec(grammar, { start: 'doc', tag: 'demo' })
    const prov = spec.meta.provenance

    for (const name of Object.keys(spec.rule)) {
      if (authored.has(name)) {
        assert.ok(!(name in prov),
          `${name} is author-written and must not be listed`)
      } else {
        assert.ok(name in prov, `generated rule ${name} has no provenance`)
        assert.ok(authored.has(prov[name]),
          `${name} resolves to ${prov[name]}, which the author never wrote`)
      }
    }
    // An entry naming a rule that was never emitted is a phantom: the
    // empty alternative of a repetition helper allocates a `$altN` name
    // and then returns without emitting anything.
    for (const name of Object.keys(prov)) {
      assert.ok(name in spec.rule, `${name} has provenance but was not emitted`)
    }
  })

  it('names the start wrapper after the start rule', () => {
    const spec = emitGrammarSpec(repeatGrammar(), { start: 'doc', tag: 'demo' })
    assert.equal(spec.meta.provenance['__start__'], 'doc')
  })

  it('survives compilation to pure and recognition specs', () => {
    // The consumer that needs provenance most loads COMPILED grammars,
    // and both shapes rebuild the spec from {options, rule} alone.
    const spec = emitGrammarSpec(repeatGrammar(),
      { start: 'doc', tag: 'demo', builtins: true })
    for (const shape of [toPureSpec, toRecognitionSpec]) {
      const out = shape(spec)
      assert.equal(out.meta.provenance['__start__'], 'doc',
        `${shape.name} dropped meta.provenance`)
    }
    // ...and through serialisation, which is how it reaches a tool.
    const text = compileSpec(spec, { strict: true, recognition: false })
    assert.equal(JSON.parse(text).meta.provenance['__start__'], 'doc')
  })

  it('can be turned off for size-sensitive embedded grammars', () => {
    const spec = emitGrammarSpec(repeatGrammar(),
      { start: 'doc', tag: 'demo', provenance: false })
    assert.equal(spec.meta, undefined)
  })
})


// Recovery sync tags on the emitted close alternates.
//
// The engine derives resynchronisation points from CLOSE alternates that
// NAME A TOKEN and carry a tag in `parse.recover.syncGroups` (shipped
// default: close, comma, end). Exactly two of this emitter's alternates
// name a token, and both are tagged — which has to be all-or-nothing,
// because the engine's structural fallback engages only while the tagged
// set is empty ACROSS THE WHOLE RULE STACK. Tagging some of them would
// switch the fallback off and silently delete the rest.
describe('sync tags', () => {
  const { Tabnas } = require('@tabnas/parser')
  const SYNC = ['close', 'comma', 'end']

  // `list = DIGIT [ "," list ]` — a tail repeat, so the emitter produces
  // its separator-continuation close alternate.
  const listSpec = () => toPureSpec(emitGrammarSpec({
    productions: [
      { name: 'doc', alts: [[ref('list')]] },
      {
        name: 'list',
        alts: [[
          { kind: 'regex', pattern: '[0-9]', flags: '' },
          {
            kind: 'opt',
            inner: { kind: 'group', alts: [[term(','), ref('list')]] },
          },
        ]],
      },
    ],
  }, { start: 'doc', tag: 'demo', builtins: true }))

  const closeAlts = (spec, rule) => {
    const c = spec.rule[rule].close
    return Array.isArray(c) ? c : (c && c.alts) || []
  }
  const tagsOf = (alt) => String(alt.g || '').split(',')

  it('tags the end-of-source anchor and the separator continuation', () => {
    const spec = listSpec()
    assert.ok(tagsOf(closeAlts(spec, '__start__').find((a) => a.s)).includes('end'))
    assert.ok(tagsOf(closeAlts(spec, 'list').find((a) => a.s)).includes('comma'))
  })

  it('tags EVERY close alternate that names a token, or none is any use', () => {
    // Partial tagging is worse than none: the first tagged alternate
    // disables the fallback for every rule on the stack, including the
    // untagged ones.
    const spec = listSpec()
    for (const rule of Object.keys(spec.rule)) {
      for (const alt of closeAlts(spec, rule)) {
        if (!alt.s) continue
        assert.ok(tagsOf(alt).some((t) => SYNC.includes(t)),
          `${rule} has a token-naming close alt with no sync group: ` +
          JSON.stringify(alt))
      }
    }
  })

  // Strip the sync groups back off, and recovery must get measurably
  // worse — otherwise the tags are decoration and this suite proves
  // nothing about them.
  const strip = (spec) => {
    const s = structuredClone(spec)
    for (const rule of Object.keys(s.rule)) {
      for (const alt of closeAlts(s, rule)) {
        if (alt && 'string' === typeof alt.g) {
          alt.g = alt.g.split(',').filter((t) => !SYNC.includes(t)).join(',')
        }
      }
    }
    return s
  }

  // A host rule with its own sync tag, standing in for the grammar this
  // one gets embedded in. Its single tag is what disables the fallback
  // the generated rules would otherwise have relied on.
  const underHost = (spec) => {
    const s = structuredClone(spec)
    s.rule.host = {
      open: [{ p: 'doc', g: 'host' }],
      close: [{ s: '#ZZ', a: '@bubble$', g: 'host,end' }],
    }
    s.options = { ...s.options, rule: { ...s.options.rule, start: 'host' } }
    return s
  }

  const recover = (spec, src) => {
    const tn = new Tabnas({ parse: { recover: { enabled: true } } })
    tn.grammar(spec)
    const out = tn.parse(src)
    return { errors: (out.errors || []).length, src: srcOf(out.value) }
  }
  const srcOf = (n) => (n && 'object' === typeof n)
    ? (undefined !== n.src ? n.src : srcOf(n.kids)) : n

  it('keeps the rest of a list when embedded in a tagged host grammar', () => {
    const spec = listSpec()
    const tagged = recover(underHost(spec), '1,!,3')
    const bare = recover(underHost(strip(spec)), '1,!,3')

    assert.equal(tagged.errors, 1)
    assert.equal(bare.errors, 1)
    // Tagged: resynchronises at the separator, so the trailing `3`
    // survives. Untagged: the host's tag has disabled the fallback, the
    // separator is no longer a sync point, and everything after the bad
    // token is lost.
    assert.match(tagged.src, /3/,
      'tagged: the item after the error should survive')
    assert.doesNotMatch(bare.src, /3/,
      'untagged: without the separator sync point the tail is lost — if ' +
      'this now passes, the tags are no longer doing anything')
  })
})


// Source spans on the IR, and the compile errors that carry them.
//
// A front-end that records where each element came from gets compile
// errors with a range, so a tool can underline the offending text
// instead of parsing it back out of the message. A front-end that
// records nothing compiles to exactly the same grammar and gets the
// same messages — every assertion below has a no-span counterpart.
describe('source spans', () => {
  const at = (s, e, r, c) => ({ s, e, r, c })

  it('carries the offending element span on an unknown rule reference', () => {
    // The most common author-facing compile error, and the one an
    // editor most wants to underline.
    const span = at(10, 15, 2, 7)
    try {
      emitGrammarSpec({
        productions: [
          { name: 'doc', alts: [[{ kind: 'ref', name: 'nope', sp: span }]] },
        ],
      }, { tag: 'demo' })
      assert.fail('expected an unknown-rule failure')
    } catch (e) {
      assert.ok(e instanceof EmitError, `expected EmitError, got ${e.name}`)
      assert.ok(e instanceof Error, 'EmitError must stay an Error')
      assert.match(e.message, /references unknown rule 'nope'/)
      assert.equal(e.rule, 'doc')
      assert.deepEqual(e.sp, span)
    }
  })

  it('still fails identically when the front-end records no spans', () => {
    try {
      emitGrammarSpec({
        productions: [{ name: 'doc', alts: [[ref('nope')]] }],
      }, { tag: 'demo' })
      assert.fail('expected an unknown-rule failure')
    } catch (e) {
      assert.ok(e instanceof EmitError)
      assert.match(e.message, /references unknown rule 'nope'/)
      assert.equal(e.sp, undefined, 'no span recorded, so none reported')
    }
  })

  it('carries the production span on a purely left-recursive rule', () => {
    const span = at(0, 12, 1, 1)
    try {
      emitGrammarSpec({
        // Every alternative re-enters the rule and consumes something,
        // so there is no seed to start from. (A bare `loop = loop` is a
        // trivial self-reference and is dropped instead.)
        productions: [{
          name: 'loop',
          alts: [[ref('loop'), term('x')], [ref('loop'), term('y')]],
          sp: span,
        }],
      }, { tag: 'demo' })
      assert.fail('expected a left-recursion failure')
    } catch (e) {
      assert.match(e.message, /purely left-recursive/)
      assert.deepEqual(e.sp, span)
      assert.equal(e.rule, 'loop')
    }
  })

  it('leaves spans on the elements the rewrite passes carry through', () => {
    // Elements are shared by reference through cloning and Paull's
    // substitution, so a span recorded at parse time survives to the
    // emitter. If that ever stops being true, the unknown-rule span
    // above would be the first casualty — this pins the mechanism
    // rather than one symptom of it.
    const span = at(20, 24, 3, 1)
    const el = { kind: 'ref', name: 'gone', sp: span }
    const grammar = {
      productions: [
        { name: 'doc', alts: [[ref('mid')]] },
        { name: 'mid', alts: [[el, term('x')]] },
      ],
    }
    try {
      emitGrammarSpec(grammar, { start: 'doc', tag: 'demo' })
      assert.fail('expected an unknown-rule failure')
    } catch (e) {
      assert.deepEqual(e.sp, span,
        'the span did not survive the rewrite passes')
    }
    // ...and the caller's own element object is untouched.
    assert.deepEqual(el.sp, span)
  })

  it('carries a production span through Paull substitution', () => {
    // `a` starts with a reference to `b`, so Paull's substitution
    // inlines `b` INTO `a` — rebuilding `a` field by field — and the
    // result is purely left-recursive, which fails carrying `a`'s span.
    // The span therefore has to survive `substituteLeadingRef`'s
    // rebuild to be reported, which the left-recursion test above does
    // not exercise: that one fails before substitution runs.
    //
    // Every rewrite pass rebuilds productions this way, and a rebuild
    // that forgets a field drops it silently. This is the shape of test
    // that catches that.
    const span = at(5, 20, 2, 1)
    try {
      emitGrammarSpec({
        productions: [
          { name: 'a', alts: [[ref('b'), term('x')]], sp: span },
          { name: 'b', alts: [[ref('a')]] },
        ],
      }, { start: 'a', tag: 'demo' })
      assert.fail('expected a left-recursion failure')
    } catch (e) {
      assert.match(e.message, /purely left-recursive/)
      assert.deepEqual(e.sp, span,
        'the span was dropped by a rewrite pass rebuilding the production')
    }
  })

  it('keeps every converted failure message byte-identical', () => {
    // EmitError changes the error's TYPE at five sites. The message is
    // the part callers have historically matched on — including this
    // repo's own suite and the front-ends' diagnostics — so it must not
    // move. Compared against the published compiler's exact strings.
    const prose = (text) => ({ kind: 'prose', text })
    const cases = [
      [{ productions: [{ name: 'doc', alts: [[ref('nope')]] }] },
        "demo: rule 'doc' references unknown rule 'nope'"],
      [{ productions: [{ name: 'loop', alts: [[ref('loop'), term('x')],
                                              [ref('loop'), term('y')]] }] },
        "demo: rule 'loop' is purely left-recursive " +
        '(no seed alternative); cannot eliminate'],
      [{ productions: [{ name: 'doc', alts: [[term('a'), prose('stuff')]] }] },
        "demo: rule 'doc' uses prose ('<stuff>') inside an expression; " +
        'prose may only stand alone as the whole definition of a ' +
        'built-in lexer token.'],
    ]
    for (const [grammar, message] of cases) {
      assert.throws(() => emitGrammarSpec(grammar, { tag: 'demo' }),
        (e) => e instanceof EmitError && e.message === message,
        `message drifted for: ${message}`)
    }
  })

  it('does not change the grammar a spanned IR compiles to', () => {
    const withSpans = {
      productions: [
        {
          name: 'doc',
          alts: [[{ kind: 'term', literal: 'a', sp: at(0, 3, 1, 1) }]],
          sp: at(0, 9, 1, 1),
        },
      ],
    }
    const without = {
      productions: [{ name: 'doc', alts: [[term('a')]] }],
    }
    const strip = (g) => toJsonic(toPureSpec(
      emitGrammarSpec(g, { tag: 'demo', builtins: true })), { strict: true })
    assert.equal(strip(withSpans), strip(without),
      'spans must be invisible in the emitted grammar')
  })
})
