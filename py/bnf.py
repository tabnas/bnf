"""bnf — reduce a serialized tabnas grammar spec to pure data.

``@tabnas/bnf`` is the shared compiler behind the BNF-family front-ends
(GBNF, ABNF, EBNF). It is a library those front-ends call rather than
something an end user drives, so this binding is deliberately narrow: it
exposes the one capability that is useful *without* a front-end —
reducing an already-serialized ``GrammarSpec`` to pure data.

Two reductions, and the difference matters::

    import bnf

    bnf.recognition_spec(spec)   # drop AST-building hooks
    bnf.pure_spec(spec)          # keep the tree builtins

``recognition_spec`` leaves a grammar that answers only "is this input in
the language" — what a validator needs. ``pure_spec`` keeps the tree
``$``-builtins, so a reloaded grammar still builds ``{rule, src, kids}``.

Both refuse a spec whose control logic is still closures: those cannot be
represented as data at all, and a reduction that dropped them silently
would hand back a grammar that no longer does what it says.

The output is a spec ``libtabnas`` can load, so the pipeline a caller
with neither Go nor Node can assemble is::

    GBNF text --libgbnf--> spec --libbnf--> recognition spec
                                                  |
                                            libtabnas --> verdicts

NOT A COMPILER ENTRY POINT. There is no "notation text in" function,
because this package parses no notation — a front-end does. For GBNF,
use the ``gbnf`` module (tabnas/gbnf), which both compiles and validates.

Build the library first::

    cd go/clib && ./build.sh

Set ``BNF_LIB`` to point at it, or pass ``path=`` to ``load()``.

A NOTE ON PROCESSES. The library carries a Go runtime, and a Go runtime
does not survive ``os.fork()`` intact. If you use ``multiprocessing``,
choose the ``spawn`` or ``forkserver`` start method rather than ``fork``.
"""

from __future__ import annotations

import ctypes
import json
import os
import platform
from typing import Any, Optional

__all__ = ["recognition_spec", "pure_spec", "BnfError", "load", "version"]


class BnfError(Exception):
    """The call itself was wrong — a spec that is not valid JSON, or one
    that cannot be represented as pure data."""


def _default_lib_path() -> str:
    if env := os.environ.get("BNF_LIB"):
        return env
    ext = {"Windows": ".dll", "Darwin": ".dylib"}.get(platform.system(), ".so")
    arch = {"x86_64": "amd64", "AMD64": "amd64", "aarch64": "arm64",
            "arm64": "arm64"}.get(platform.machine(), platform.machine())
    goos = {"Windows": "windows", "Darwin": "darwin"}.get(
        platform.system(), "linux")
    here = os.path.dirname(os.path.abspath(__file__))
    for candidate in (
        os.path.join(here, f"libbnf{ext}"),
        os.path.join(here, "..", "go", "clib", "dist",
                     f"libbnf-{goos}-{arch}{ext}"),
    ):
        if os.path.exists(candidate):
            return candidate
    raise BnfError(
        "cannot find the bnf shared library. Build it with "
        "`cd go/clib && ./build.sh`, then set BNF_LIB to the result "
        "or pass path= to bnf.load()."
    )


_lib = None


def load(path: Optional[str] = None):
    """Load the shared library. Called automatically on first use.

    An explicit ``path`` is remembered, so ``bnf.load(path=...)``
    followed by a plain ``recognition_spec(...)`` works — caching only
    the auto-discovered library would send the second call back to
    discovery and fail for anyone whose library is not on the default
    search path.
    """
    global _lib
    if _lib is not None and path is None:
        return _lib

    lib = ctypes.CDLL(path or _default_lib_path())

    # Returned strings are ours to free, so they come back as void* —
    # ctypes would otherwise copy a c_char_p and lose the pointer we
    # have to hand to bnf_free.
    lib.bnf_version.restype = ctypes.c_void_p
    lib.bnf_version.argtypes = []
    for name in ("bnf_recognition_spec", "bnf_pure_spec"):
        fn = getattr(lib, name)
        fn.restype = ctypes.c_void_p
        fn.argtypes = [ctypes.c_char_p, ctypes.c_int]
    lib.bnf_free.restype = None
    lib.bnf_free.argtypes = [ctypes.c_void_p]

    _lib = lib
    return lib


def _call(lib, ptr) -> dict:
    """Decode a returned document and release it."""
    if not ptr:
        raise BnfError("the library returned nothing")
    try:
        return json.loads(ctypes.string_at(ptr).decode("utf-8"))
    finally:
        lib.bnf_free(ptr)


def version() -> dict:
    """The compiler and engine versions behind the loaded library."""
    lib = load()
    res = _call(lib, lib.bnf_version())
    return {"bnf": res.get("version"), "engine": res.get("engine")}


def _reduce(fn_name: str, spec: Any, path: Optional[str], as_text: bool):
    lib = load(path)
    if isinstance(spec, (dict, list)):
        spec = json.dumps(spec)
    if isinstance(spec, str):
        spec = spec.encode("utf-8")
    if not isinstance(spec, (bytes, bytearray)):
        raise TypeError("spec must be a dict, str or bytes")
    spec = bytes(spec)

    res = _call(lib, getattr(lib, fn_name)(spec, len(spec)))
    if not res.get("ok"):
        raise BnfError(res.get("error", {}).get("message", "reduction failed"))
    out = res["spec"]
    return out if as_text else json.loads(out)


def recognition_spec(spec: Any, *, path: Optional[str] = None,
                     as_text: bool = False):
    """Reduce a spec to a function-free RECOGNITION grammar.

    The AST-building hooks are dropped; what remains decides only whether
    input is in the language. Returns a dict, or the spec text when
    ``as_text`` is set — text is the form to hand to ``libtabnas``, since
    a regex travels as an ``@/src/flags`` sentinel that a re-encode
    through ``json.dumps`` would preserve but a naive one might not.
    """
    return _reduce("bnf_recognition_spec", spec, path, as_text)


def pure_spec(spec: Any, *, path: Optional[str] = None,
              as_text: bool = False):
    """Reduce a spec to pure data, KEEPING the tree-building builtins, so
    a reloaded grammar still builds ``{rule, src, kids}``."""
    return _reduce("bnf_pure_spec", spec, path, as_text)
