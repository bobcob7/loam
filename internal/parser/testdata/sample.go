package sample

// Greeter says hello.
type Greeter struct {
	Name string
}

// Greet returns a greeting for the Greeter's Name.
func (g Greeter) Greet() string {
	return "hello, " + g.Name
}

func add(a, b int) int {
	return a + b
}
