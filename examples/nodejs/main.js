// A plain CommonJS script: it require()s the real lodash and commander
// packages and prints a deterministic JSON summary.
const _ = require("lodash");
const { Command } = require("commander");

const program = new Command();
program.name("demo").option("-n, --name <who>", "who to greet", "world");
program.parse(["node", "demo", "--name", "carol"]);

const users = [
	{ name: "carol", age: 34 },
	{ name: "alice", age: 30 },
	{ name: "bob", age: 30 },
];
console.log(JSON.stringify({
	greeting: "hello " + program.opts().name,
	names: _.map(users, "name").sort(),
	byAge: _.mapValues(_.groupBy(users, "age"), (g) => g.length),
}));
