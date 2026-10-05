package main

import (
	"fmt"
	"os"

	_ "github.com/BurntSushi/toml"
)

var version = "dev"
var commands = make(map[string]func([]string) int)

// Register adds a subcommand to the driver.
func Register(name string, run func(args []string) int) {
	commands[name] = run
}

func init() {
	Register("version", func(args []string) int {
		fmt.Printf("orca-factory-driver %s\n", version)
		return 0
	})
}

func run(args []string) int {
	if len(args) > 0 {
		if command, ok := commands[args[0]]; ok {
			return command(args[1:])
		}
	}
	fmt.Fprintln(os.Stderr, "Usage: orca-factory-driver <command> [args]")
	return 2
}

func main() {
	os.Exit(run(os.Args[1:]))
}
