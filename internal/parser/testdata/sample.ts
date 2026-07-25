interface Greeting {
  name: string;
}

class Greeter implements Greeting {
  name: string;

  constructor(name: string) {
    this.name = name;
  }

  greet(): string {
    return `hello, ${this.name}`;
  }
}

function add(a: number, b: number): number {
  return a + b;
}
