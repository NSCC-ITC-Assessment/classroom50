"""Reference solution — passes every declarative test in tests.json."""


def greet(name: str) -> str:
    return f"hello, {name}!"


if __name__ == "__main__":
    print(greet(input()))
