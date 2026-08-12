"""The Python binding, checked against the property that matters.

Run after building the library:

    cd go/clib && ./build.sh
    cd ../../py && python3 -m unittest -v

The point of a reduction is not that it produces valid JSON — it is that
what comes out is still a WORKING grammar. Where a sibling checkout of
tabnas/parser is present these tests prove that end to end by reloading
the result; otherwise they check the surface and skip the rest rather
than assert something weaker while claiming something stronger.
"""

import json
import os
import unittest

import bnf

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)

# A spec with a TREE builtin, which is what separates the two
# reductions. Small enough to read, and it exercises the difference the
# API exists to offer.
TREE_SPEC = {"rule": {"val": {"open": [{"s": ["#NR"], "a": "@node$"}]}}}


def engine_fixture():
    for p in (
        os.path.join(ROOT, "..", "parser", "ts", "test",
                     "json-builder.fixture.json"),
        os.path.join(ROOT, "test", "json-builder.fixture.json"),
    ):
        if os.path.exists(p):
            with open(p, "rb") as f:
                return f.read()
    return None


class TestSurface(unittest.TestCase):
    def test_version(self):
        v = bnf.version()
        self.assertRegex(v["bnf"], r"^\d+\.\d+\.\d+")
        self.assertRegex(v["engine"], r"^\d+\.\d+\.\d+")

    def test_accepts_dict_str_and_bytes(self):
        want = bnf.recognition_spec(TREE_SPEC)
        for form in (json.dumps(TREE_SPEC), json.dumps(TREE_SPEC).encode()):
            self.assertEqual(bnf.recognition_spec(form), want)

    def test_as_text_returns_the_spec_text(self):
        text = bnf.recognition_spec(TREE_SPEC, as_text=True)
        self.assertIsInstance(text, str)
        self.assertEqual(json.loads(text), bnf.recognition_spec(TREE_SPEC))

    def test_a_bad_spec_raises(self):
        for bad in ("{not a spec", '{"rule":'):
            with self.assertRaises(BnfErrorAlias):
                bnf.recognition_spec(bad)

    def test_type_error_for_nonsense(self):
        with self.assertRaises(TypeError):
            bnf.recognition_spec(42)

    def test_explicit_path_is_remembered(self):
        lib = bnf._default_lib_path()
        bnf._lib = None
        try:
            bnf.load(lib)
            self.assertTrue(bnf.recognition_spec(TREE_SPEC))
        finally:
            bnf._lib = None


BnfErrorAlias = bnf.BnfError


class TestReductions(unittest.TestCase):
    def test_recognition_drops_tree_builtins_pure_keeps_them(self):
        rec = bnf.recognition_spec(TREE_SPEC, as_text=True)
        pure = bnf.pure_spec(TREE_SPEC, as_text=True)
        self.assertNotIn("@node$", rec)
        self.assertIn("@node$", pure)

    def test_reduced_spec_is_still_a_working_grammar(self):
        """The property the whole API rests on.

        Reducing a spec is only useful if the result still parses what
        the original parsed. Needs the engine's Python binding to check,
        which lives in the sibling tabnas/parser checkout.
        """
        raw = engine_fixture()
        if raw is None:
            self.skipTest("no serialized spec fixture available")
        try:
            import sys
            sys.path.insert(0, os.path.join(ROOT, "..", "parser", "py"))
            import tabnas
        except Exception as e:  # pragma: no cover - environment dependent
            self.skipTest(f"engine binding unavailable: {e}")

        reduced = bnf.recognition_spec(raw, as_text=True)
        with tabnas.Grammar(reduced) as g:
            self.assertTrue(g.accepts('{"a":1}'))
            self.assertTrue(g.accepts('{"a":1,"b":[1,2]}'))
            self.assertFalse(g.accepts('{"a":1,}'))
            self.assertFalse(g.accepts("{oops"))


if __name__ == "__main__":
    unittest.main()
