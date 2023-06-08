import os


def umask():
    """Get umask without changing it."""
    old = os.umask(0)
    os.umask(old)
    return old


def tostr(s, encoding="utf-8") -> str:
    """Automate unicode conversion"""
    if isinstance(s, str):
        return s
    if hasattr(s, "decode"):
        return s.decode(encoding)
    raise ValueError("Cound not decode")


def tobytes(s, encoding="utf8") -> bytes:
    """Automatic byte conversion"""
    if isinstance(s, bytes):
        return s
    if hasattr(s, "encode"):
        return s.encode(encoding)
    raise ValueError("Could not encode")
