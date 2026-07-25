"""Mutual-recursion fixture: is_even and is_odd call each other.

Exercises the dependents recursive-CTE's cycle-safety requirement: resolving
either function's dependents must terminate rather than loop forever.
"""


def is_even(n: int) -> bool:
    """Return True if n is even, deferring to is_odd for the recursive case."""
    if n == 0:
        return True
    return is_odd(n - 1)


def is_odd(n: int) -> bool:
    """Return True if n is odd, deferring to is_even for the recursive case."""
    if n == 0:
        return False
    return is_even(n - 1)
