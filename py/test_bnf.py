"""The Python binding, checked against the property that matters.

Run after building the library:

    cd go/clib && ./build.sh
    cd ../../py && python3 -m unittest -v

The point of a reduction is not that it produces valid JSON — it is that
what comes out is still a WORKING grammar. Where the engine's own C
library is at hand (``TABNAS_LIB``, or a built sibling checkout of
tabnas/parser) these tests prove that end to end by reloading the
result; otherwise they check the surface and skip the rest rather than
assert something weaker while claiming something stronger.
"""

import ctypes
import json
import os
import platform
import unittest

import bnf

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)

# A spec with a TREE builtin, which is what separates the two
# reductions. Small enough to read, and it exercises the difference the
# API exists to offer.
TREE_SPEC = {"rule": {"val": {"open": [{
    "s": ["#NR"], "a": "@node$",
    "k": {"node$": {"init": True, "rule": "val", "nterms": 1}},
}]}}}

# Action 1 is not a builtin name, so it stands for a closure: control
# logic that cannot be data.
CLOSURE_SPEC = {"rule": {"val": {"open": [{"s": ["#NR"], "a": 1}]}}}


def reset():
    """Forget the loaded library, releasing its handle."""
    if bnf._lib is not None and bnf._handle is not None:
        bnf._lib.tabnas_grammar_free(bnf._handle)
    bnf._lib = bnf._handle = bnf._path = None


def engine_fixture():
    p = os.path.join(ROOT, "..", "parser", "ts", "test",
                     "json-builder.fixture.json")
    if os.path.exists(p):
        with open(p, "rb") as f:
            return f.read()
    return None


def engine_lib_path():
    if env := os.environ.get("TABNAS_LIB"):
        return env
    ext = {"Windows": ".dll", "Darwin": ".dylib"}.get(platform.system(), ".so")
    arch = {"x86_64": "amd64", "AMD64": "amd64", "aarch64": "arm64",
            "arm64": "arm64"}.get(platform.machine(), platform.machine())
    goos = {"Windows": "windows", "Darwin": "darwin"}.get(
        platform.system(), "linux")
    p = os.path.join(ROOT, "..", "parser", "go", "clib", "dist",
                     f"libtabnasparser-{goos}-{arch}{ext}")
    return p if os.path.exists(p) else None


class Engine:
    """The engine's library, over the same five uniform symbols: its
    tabnas_grammar takes a serialized spec, and tabnas_parse gives the
    verdict."""

    def __init__(self, path):
        self.lib = ctypes.CDLL(path)
        bnf._bind(self.lib)
        info = bnf._call(self.lib, self.lib.tabnas_version())
        if info.get("format") != "parser":
            raise RuntimeError(f"{path} is not the engine library: {info}")

    def load(self, spec_text):
        b = spec_text.encode("utf-8")
        res = bnf._call(self.lib, self.lib.tabnas_grammar(b, len(b)))
        if not res.get("ok"):
            raise AssertionError(f"reduced spec will not load: {res}")
        return res["handle"]

    def parse(self, handle, src):
        b = src.encode("utf-8")
        return bnf._call(self.lib, self.lib.tabnas_parse(handle, b, len(b)))

    def free(self, handle):
        self.lib.tabnas_grammar_free(handle)


def engine(test):
    path = engine_lib_path()
    if path is None:
        test.skipTest("no engine library: set TABNAS_LIB, or build "
                      "../parser/go/clib")
    try:
        return Engine(path)
    except Exception as e:  # pragma: no cover - environment dependent
        test.skipTest(f"engine library unusable: {e}")


class TestSurface(unittest.TestCase):
    def test_version(self):
        v = bnf.version()
        self.assertEqual(v["lib"], "libtabnasbnf")
        self.assertEqual(v["format"], "bnf")
        self.assertRegex(v["template"], r"^v\d+$")

    def test_accepts_dict_str_and_bytes(self):
        want = bnf.recognition_spec(TREE_SPEC)
        for form in (json.dumps(TREE_SPEC), json.dumps(TREE_SPEC).encode()):
            self.assertEqual(bnf.recognition_spec(form), want)

    def test_as_text_returns_the_spec_text(self):
        text = bnf.recognition_spec(TREE_SPEC, as_text=True)
        self.assertIsInstance(text, str)
        self.assertEqual(json.loads(text), bnf.recognition_spec(TREE_SPEC))

    def test_a_bad_spec_raises(self):
        # The library answers these (accept:false); the call did not
        # fail, so there is no code.
        for bad in ("{not a spec", '{"rule":', "", "[1]"):
            with self.assertRaises(bnf.BnfError) as cm:
                bnf.recognition_spec(bad)
            self.assertIsNone(cm.exception.code, bad)
            self.assertTrue(str(cm.exception), bad)

    def test_a_closure_spec_names_its_rules(self):
        for reduce in (bnf.recognition_spec, bnf.pure_spec):
            with self.assertRaises(bnf.BnfError) as cm:
                reduce(CLOSURE_SPEC)
            self.assertIsNone(cm.exception.code)
            self.assertEqual(cm.exception.rules, ["val"])

    def test_a_failed_call_carries_its_code(self):
        lib = bnf.load()
        b = b"{}"
        res = bnf._call(lib, lib.tabnas_parse(1 << 40, b, len(b)))
        self.assertFalse(res["ok"])
        err = bnf._error(res)
        self.assertEqual(err.code, "handle")
        self.assertTrue(str(err))

    def test_type_error_for_nonsense(self):
        with self.assertRaises(TypeError):
            bnf.recognition_spec(42)

    def test_explicit_path_is_remembered(self):
        lib = bnf._default_lib_path()
        reset()
        try:
            bnf.load(lib)
            self.assertTrue(bnf.recognition_spec(TREE_SPEC))
            self.assertEqual(bnf._path, lib)
        finally:
            reset()


class TestReductions(unittest.TestCase):
    def test_recognition_drops_builders_pure_keeps_them(self):
        spec = {"rule": {
            "val": {"open": [
                {"s": ["#NR"], "a": "@node$"},
                {"s": ["#OB"], "p": "map", "a": "@object$"},
            ]},
            "map": {"close": [{"s": ["#CB"], "a": "@setval$"}]},
        }}
        rec = bnf.recognition_spec(spec, as_text=True)
        pure = bnf.pure_spec(spec, as_text=True)
        for builder in ("@node$", "@object$", "@setval$"):
            self.assertNotIn(builder, rec)
            self.assertIn(builder, pure)

    def test_array_form_tokens_survive(self):
        # The library before the uniform ABI wrote these as null.
        for reduce in (bnf.recognition_spec, bnf.pure_spec):
            alt = reduce(TREE_SPEC)["rule"]["val"]["open"][0]
            self.assertEqual(alt["s"], ["#NR"])

    def test_reduced_spec_is_still_a_working_grammar(self):
        """The property the whole API rests on.

        Reducing a spec is only useful if the result still parses what
        the original parsed. Needs the engine's library to check, and
        the engine's JSON-builder fixture from a sibling checkout.
        """
        raw = engine_fixture()
        if raw is None:
            self.skipTest("no serialized spec fixture available")
        eng = engine(self)

        for reduce in (bnf.recognition_spec, bnf.pure_spec):
            h = eng.load(reduce(raw, as_text=True))
            try:
                for src, want in (('{"a":1}', True),
                                  ('{"a":1,"b":[1,2]}', True),
                                  ('{"a":1,}', False),
                                  ("{oops", False)):
                    res = eng.parse(h, src)
                    self.assertTrue(res["ok"], res)
                    self.assertEqual(res["accept"], want,
                                     f"{reduce.__name__}: {src!r} {res}")
            finally:
                eng.free(h)

    def test_only_pure_still_builds_a_value(self):
        eng = engine(self)
        rec = eng.load(bnf.recognition_spec(TREE_SPEC, as_text=True))
        pure = eng.load(bnf.pure_spec(TREE_SPEC, as_text=True))
        try:
            for h in (rec, pure):
                self.assertTrue(eng.parse(h, "1")["accept"])
                self.assertFalse(eng.parse(h, '"x"')["accept"])
            self.assertIsNone(eng.parse(rec, "1").get("value"))
            self.assertIsNotNone(eng.parse(pure, "1").get("value"))
        finally:
            eng.free(rec)
            eng.free(pure)

    def test_the_engine_library_is_not_mistaken_for_bnf(self):
        engine(self)  # skips unless a usable engine library is at hand
        path = engine_lib_path()
        reset()
        try:
            with self.assertRaises(bnf.BnfError):
                bnf.load(path)
            self.assertIsNone(bnf._lib)
        finally:
            reset()


if __name__ == "__main__":
    unittest.main()
