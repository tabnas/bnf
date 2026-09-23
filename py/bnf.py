"""bnf — reduce a serialized tabnas grammar spec to pure data.

``@tabnas/bnf`` is the shared compiler behind the BNF-family front-ends
(GBNF, ABNF, EBNF). It is a library those front-ends call rather than
something an end user drives, so this binding is deliberately narrow: it
exposes the one capability that is useful *without* a front-end —
reducing an already-serialized ``GrammarSpec`` to pure data.

Two reductions, and the difference matters::

    import bnf

    bnf.recognition_spec(spec)   # drop every output builder
    bnf.pure_spec(spec)          # keep them

``recognition_spec`` leaves a grammar that answers only "is this input in
the language" — what a validator needs. It drops the tree builtins
(``@node$``, ``@capture$``, …) and the value builders (``@object$``,
``@array$``, …) alike, so a reloaded grammar builds nothing.
``pure_spec`` keeps them, so a reloaded grammar still builds its value.

Both refuse a spec whose control logic is still closures: those cannot be
represented as data at all, and a reduction that dropped them silently
would hand back a grammar that no longer does what it says.

The output is a spec the engine's own C library (tabnas/parser
``go/clib``) loads, so the pipeline a caller with neither Go nor Node can
assemble is::

    front-end --> spec --libtabnasbnf--> recognition spec
                                              |
                                  engine library --> verdicts

NOT A COMPILER ENTRY POINT. There is no "notation text in" function,
because this package parses no notation — a front-end does. For GBNF,
use the ``gbnf`` module (tabnas/gbnf), which both compiles and validates.

THE LIBRARY. ``libtabnasbnf`` exports the uniform tabnas C ABI (admin
ADR-12) — ``tabnas_version``, ``tabnas_grammar``, ``tabnas_parse``,
``tabnas_grammar_free`` and ``tabnas_free`` — like every per-format
tabnas library. The spec is its parse input, and the parse value
carries both reductions at once; this module picks one. Because every
format library exports the same symbols, :func:`load` checks that the
library it opened reports format ``bnf``: a GBNF library loaded by
mistake would otherwise answer with GBNF verdicts.

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
import threading
from typing import Any, Optional

__all__ = ["recognition_spec", "pure_spec", "BnfError", "load", "version"]

FORMAT = "bnf"


class BnfError(Exception):
    """A spec that cannot be reduced, or a call that failed.

    ``code`` is set when the library reports that the call itself failed
    (``ok:false``: ``handle``, ``usage``, ``grammar``, ``internal``). It
    is ``None`` when the library read the spec and rejected it — not
    valid JSON, or control logic that is still closures — and then
    ``rules`` names the rules that still need closures, if that is why.
    """

    def __init__(self, message: str, *, code: Optional[str] = None,
                 rules: Optional[list] = None):
        super().__init__(message)
        self.code = code
        self.rules = list(rules or [])


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
        os.path.join(here, f"libtabnasbnf{ext}"),
        os.path.join(here, "..", "go", "clib", "dist",
                     f"libtabnasbnf-{goos}-{arch}{ext}"),
    ):
        if os.path.exists(candidate):
            return candidate
    raise BnfError(
        "cannot find the bnf shared library. Build it with "
        "`cd go/clib && ./build.sh`, then set BNF_LIB to the result "
        "or pass path= to bnf.load()."
    )


def _bind(lib) -> None:
    """Declare the five uniform symbols on a loaded library.

    Returned strings are ours to free, so they come back as void* —
    ctypes would otherwise copy a c_char_p and lose the pointer we have
    to hand to tabnas_free.
    """
    lib.tabnas_version.restype = ctypes.c_void_p
    lib.tabnas_version.argtypes = []
    lib.tabnas_grammar.restype = ctypes.c_void_p
    lib.tabnas_grammar.argtypes = [ctypes.c_char_p, ctypes.c_int]
    lib.tabnas_parse.restype = ctypes.c_void_p
    lib.tabnas_parse.argtypes = [ctypes.c_longlong, ctypes.c_char_p,
                                 ctypes.c_int]
    lib.tabnas_grammar_free.restype = None
    lib.tabnas_grammar_free.argtypes = [ctypes.c_longlong]
    lib.tabnas_free.restype = None
    lib.tabnas_free.argtypes = [ctypes.c_void_p]


def _call(lib, ptr) -> dict:
    """Decode a returned document and release it."""
    if not ptr:
        raise BnfError("the library returned nothing")
    try:
        return json.loads(ctypes.string_at(ptr).decode("utf-8"))
    finally:
        lib.tabnas_free(ptr)


def _error(res: dict) -> BnfError:
    """The BnfError for a failed call (ok:false) or a rejection."""
    err = res.get("error")
    if not isinstance(err, dict):
        err = {}
    failed = not res.get("ok")
    # A failed call carries {code, message}; a rejection carries the
    # compiler's {Message, Rules}.
    message = (err.get("message") or err.get("Message")
               or ("call failed" if failed else "spec rejected"))
    return BnfError(message,
                    code=err.get("code", "unknown") if failed else None,
                    rules=err.get("Rules"))


_lib = None
_handle = None
_path = None
_lock = threading.Lock()


def _open(path: Optional[str]):
    """The loaded library and its handle, loading on first use."""
    global _lib, _handle, _path
    with _lock:
        if _lib is not None and (path is None or path == _path):
            return _lib, _handle

        where = path or _default_lib_path()
        lib = ctypes.CDLL(where)
        try:
            _bind(lib)
        except AttributeError as e:
            # A libtabnasbnf built before the uniform ABI exported
            # bnf_* symbols instead, and may still sit beside this file.
            raise BnfError(
                f"{where} does not export the uniform tabnas C ABI ({e}); "
                "rebuild it with `cd go/clib && ./build.sh`") from None

        info = _call(lib, lib.tabnas_version())
        if info.get("format") != FORMAT:
            raise BnfError(
                f"{where} is {info.get('lib') or 'not a tabnas library'} "
                f"(format {info.get('format')!r}), not libtabnasbnf")

        # One handle serves every call. The library gives each handle
        # its own mutex, so sharing it across threads is safe.
        res = _call(lib, lib.tabnas_grammar(None, 0))
        if not res.get("ok"):
            raise _error(res)

        if _lib is not None and _handle is not None:
            _lib.tabnas_grammar_free(_handle)
        _lib, _handle, _path = lib, res["handle"], path
        return _lib, _handle


def load(path: Optional[str] = None):
    """Load the shared library. Called automatically on first use.

    An explicit ``path`` is remembered, so ``bnf.load(path=...)``
    followed by a plain ``recognition_spec(...)`` works — caching only
    the auto-discovered library would send the second call back to
    discovery and fail for anyone whose library is not on the default
    search path.

    Raises :class:`BnfError` when the library found is not
    ``libtabnasbnf``.
    """
    return _open(path)[0]


def version() -> dict:
    """What the loaded library reports about itself.

    ``{"lib": "libtabnasbnf", "format": "bnf", "template": "v3"}``: the
    library, its format, and the clib template it was built from. The
    uniform ABI reports neither the compiler's nor the engine's version.
    """
    lib = load()
    res = _call(lib, lib.tabnas_version())
    return {k: v for k, v in res.items() if k != "ok"}


def _reduce(key: str, spec: Any, path: Optional[str], as_text: bool):
    if isinstance(spec, (dict, list)):
        spec = json.dumps(spec)
    if isinstance(spec, str):
        spec = spec.encode("utf-8")
    if not isinstance(spec, (bytes, bytearray)):
        raise TypeError("spec must be a dict, str or bytes")
    spec = bytes(spec)

    lib, handle = _open(path)
    res = _call(lib, lib.tabnas_parse(handle, spec, len(spec)))
    if not res.get("ok") or not res.get("accept"):
        raise _error(res)
    if "value" not in res:
        raise BnfError(res.get("valueError", "the library returned no spec"))
    out = res["value"][key]
    return json.dumps(out) if as_text else out


def recognition_spec(spec: Any, *, path: Optional[str] = None,
                     as_text: bool = False):
    """Reduce a spec to a function-free RECOGNITION grammar.

    Every output builder is dropped; what remains decides only whether
    input is in the language. Returns a dict, or the spec as JSON text
    when ``as_text`` is set — the form to hand to the engine's library.
    A regex travels as an ``@/source/flags`` string, which the engine
    decodes on load; keep those strings intact if you re-serialize.
    """
    return _reduce("recognition", spec, path, as_text)


def pure_spec(spec: Any, *, path: Optional[str] = None,
              as_text: bool = False):
    """Reduce a spec to pure data, KEEPING the output builders, so a
    reloaded grammar still builds its value."""
    return _reduce("pure", spec, path, as_text)
