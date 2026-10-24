"""Errors with cgraph's command-line exit-status semantics."""

from __future__ import annotations


class InputError(RuntimeError):
    """An input that cannot be read or validated."""


class CacheError(InputError):
    """A cache manifest or facts entry is missing or malformed."""


class ExtractionError(InputError):
    """libclang could not extract a translation unit's facts."""


class WorkerDied(RuntimeError):
    """A fact-extraction worker exited before it returned its result."""
