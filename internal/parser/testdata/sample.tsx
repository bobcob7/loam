interface GreetingProps {
  name: string;
}

function Greeting({ name }: GreetingProps) {
  return <div>hello, {name}</div>;
}

export default Greeting;
