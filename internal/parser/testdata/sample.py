class Greeter:
    """Says hello."""

    def __init__(self, name):
        self.name = name

    def greet(self):
        return f"hello, {self.name}"


def add(a, b):
    return a + b
