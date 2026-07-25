class Greeter {
  constructor(name) {
    this.name = name;
  }

  greet() {
    return `hello, ${this.name}`;
  }
}

function add(a, b) {
  return a + b;
}

module.exports = { Greeter, add };
