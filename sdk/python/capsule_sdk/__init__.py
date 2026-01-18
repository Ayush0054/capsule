"""
Capsule SDK - Python Client

A client for interacting with the Capsule API server.
Isolated. Contained. Secure.
"""

from .client import (
    CapsuleClient,
    Capsule,
    ExecResult,
    FileInfo,
    CapsuleError,
)

__all__ = [
    "CapsuleClient",
    "Capsule",
    "ExecResult",
    "FileInfo",
    "CapsuleError",
]

__version__ = "0.1.0"
