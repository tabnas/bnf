# The schema `grammar-rules`

`grammar-rules` is a grammar's rules as its text writes them: uncompiled,
and in source order. The three front ends that build on this package,
[ABNF](https://github.com/tabnas/abnf), [EBNF](https://github.com/tabnas/ebnf)
and [GBNF](https://github.com/tabnas/gbnf), read their documents as this
tree and write it, so a translation reads a grammar in one notation and
writes it in another. It is the schema step 23 of alchemy's streaming plan
records (`docs/streaming-plan.md`, section 13.1, in tabnas/alchemy), from
ruling 4 of 2026-11-08 (admin ADR-33). The compiled `GrammarSpec`, which a
host installs on an engine, is another thing and stays as it is.

## The tree

```text
{ "type": "grammar", ["start": name], "rules": [ rule, ... ] }

rule     { "type": "rule", "name": ..., ["incremental": true], "alternatives": [ alternative, ... ] }
alternative   [ element, ... ]
element  { "type": ..., ...its members, ["repeat": [ repetition, ... ]] }
repetition    { "min": m, ["max": n] }
```

Every object's first member is `type`, and its other members follow in
the order this page lists them; a member in brackets is there only where
the text writes it. The order is part of the schema: a reader streams a
tree as its parser's rule passes reach each member, so no member waits for
one the text spells later.

`start` names the rule the grammar starts from, where its notation names
one whatever the rules' order: GBNF starts from the rule named `root`, so
its tree is `{"type": "grammar", "start": "root", ...}`, `start` known
before any rule is read. ABNF's and EBNF's trees have no `start`: they
start from their first rule. A rule of that name need not be among the
rules (a GBNF document can lack its `root`), and the check below does not
look for one.

A rule is an entry for each definition, where the text writes it. ABNF's
`a =/ "c"` is an entry of its own, `incremental` after its name, after the
`a = ...` it extends; a compiler merges the two, as the ABNF front end does.
An alternative is a concatenation, a list of elements, and the empty list
is the empty alternative (GBNF's `ws ::= | " "`). A rule and a group each
hold at least one alternative.

| `type` | Members | ABNF | GBNF | EBNF |
|---|---|---|---|---|
| `reference` | `name` | `name` | `name` | `name` |
| `literal` | `text`, `caseSensitive` | `"ab"`, `%i"ab"` (false); `%s"ab"`, `%x61.62` (true) | `"ab"` (true) | `"ab"`, `'ab'` (true) |
| `char` | `value` | `%x61` | | `#x61` |
| `range` | `from`, `to` | `%x61-7A` | | |
| `class` | `negated`, `members` | | `[a-z]`, `[^"]` | `[a-z]`, `[^<&]`, `[#x20-#x7E]` |
| `any` | | | `.` | |
| `token` | `negated`, `text` | | `<think>`, `!<[1000]>` | |
| `prose` | `text` | `<a number>` | | |
| `group` | `alternatives` | `( ... )`, `[ ... ]` | `( ... )` | `( ... )` |

A code point is a number from 0 to 0x10FFFF: a `char`'s `value`, a
`range`'s `from` and `to`, and a class's `members`, each
`{ "from", "to" }`, a lone character a member whose bounds are one. So a
character means the same whatever notation spelled it. A literal's `text`
is the characters it matches, its escapes read, and `caseSensitive` says
whether it matches them in other case too, as ABNF's quoted strings do.

An element may end with `repeat`, the repetitions applied to it, the
innermost first. A repetition's `min` and `max` are whole numbers, `max`
left out where the repetition is unbounded and never less than `min`.
GBNF's and EBNF's operators (`?`, `*`, `+`, `{m,n}`) follow their element,
so a repetition is its element's last member; ABNF's prefix (`*`, `m*n`,
`n`) is written there too. An optional is the repetition from 0 to 1 in
every notation: ABNF's `[ a / b ]` is a group with that repetition, as
`*1( a / b )` is, and GBNF's and EBNF's `x?` is `x` with it.

The tree holds the rules a document writes, and nothing a compiler adds:
not ABNF's core rules, not a compiler's start wrapper, not a compile's
options.
Comments are not kept, and neither is ABNF's value annotation, which a
comment carries.

## What each notation can write

Each front end writes the tree in its own notation, as far as the
notation can say it, and refuses the rest with
TARGET_VALUE_UNREPRESENTABLE, naming the rule and what it met. What one
notation spells another way is written that way: a case-insensitive
literal as the classes that match it, a range or a character as a class,
a class as the ranges and characters it holds. What a notation has no
form for is refused: prose and `=/` in GBNF and EBNF, a token terminal in
ABNF and EBNF, and a name the notation cannot spell.

The start rule goes where the target reads it. A notation that starts
from its first rule (ABNF, EBNF), given a `start` that is not its first
rule, holds the rules before it, writes the start rule first and streams
the rest: what is held is the text of the rules before the start rule,
and nothing is held when the start rule comes first or the tree has no
`start`. A `start` naming a rule that never arrives is refused at the
grammar's end, naming the rule, rather than written as a grammar that
starts elsewhere. GBNF, which starts from `root`, writes a tree with no
`start` (one that starts from its first rule) with `root ::= ` and that
rule's name added after the others, and refuses one whose rule named
`root` is not its first, since GBNF would start from that rule instead.

## The check

`checkRules` in TypeScript, `CheckRules` in Go and `check_rules` in Rust
hold a value to the schema, its objects' members in their order, and fail
with a `RulesError` whose `code` says what was wrong and whose `path` says
where:

| `code` | Raised when |
|---|---|
| `rules_root` | the root is not a grammar |
| `rules_type` | a rule or an element whose first member is not a type it has |
| `rules_member` | a member the node does not have, one it lacks, or one out of its order or repeated |
| `rules_value` | a member whose value is not what the schema gives it: a name or a `start` that is empty, a code point past 0x10FFFF, a range or a span whose `to` is less than its `from`, a list of alternatives or repetitions that is empty |

The code is the contract; the message is informative.
[`test/spec/rules.tsv`](../test/spec/rules.tsv) holds the three runtimes
to the same answers. In Go, a tree's objects are held in order where they
are `*tabnas.OrderedMap`, as a front end builds them; a plain
`map[string]any` has no order to hold, so its members and their values
are checked and their order is not.
