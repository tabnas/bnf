/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */
'use strict'

// The schema grammar-rules, held to the shared fixtures in
// test/spec/rules.tsv, which go/rules_test.go and rs/tests/rules_test.rs
// run too: each tree is the schema's or refused with the code its row
// names.

const { describe, it } = require('node:test')
const assert = require('node:assert')
const fs = require('node:fs')
const path = require('node:path')

const { checkRules, RulesError } = require('..')

const file = path.join(__dirname, '..', '..', 'test', 'spec', 'rules.tsv')
const lines = fs.readFileSync(file, 'utf8').split('\n')

describe('grammar-rules, test/spec/rules.tsv', () => {
  let rows = 0
  lines.slice(1).forEach((line, i) => {
    if ('' === line || (line.startsWith('#') && !line.includes('\t'))) return
    const [input, expected] = line.split('\t')
    rows++
    it(`rules.tsv:${i + 2}`, () => {
      const tree = JSON.parse(input)
      if ('OK' === expected) {
        checkRules(tree)
        return
      }
      assert.throws(() => checkRules(tree), (err) => {
        assert.ok(err instanceof RulesError, String(err))
        assert.equal(`ERROR:${err.code}`, expected, err.message)
        return true
      })
    })
  })
  it('runs every row', () => assert.equal(rows, 39))
})
