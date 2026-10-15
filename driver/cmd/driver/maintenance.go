package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
)

func init() { Register("maintenance", runMaintenance) }
func runMaintenance(args []string) int {
	if len(args) > 1 && args[0] == "exec" {
		release, err := maintenance.Write()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer release()
		command := exec.Command(args[1], args[2:]...)
		command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err = command.Run(); err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				return exit.ExitCode()
			}
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	const usage = "usage: driver maintenance status|freeze|drain [--wait 2m]|resume [--wait 2m]"
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	var wait time.Duration
	if args[0] == "drain" || args[0] == "resume" {
		flags := flag.NewFlagSet("maintenance "+args[0], flag.ContinueOnError)
		flags.SetOutput(os.Stderr)
		flags.DurationVar(&wait, "wait", 2*time.Minute, "maximum wait for admitted claims and dispatches")
		if flags.Parse(args[1:]) != nil || flags.NArg() != 0 || wait < 0 {
			fmt.Fprintln(os.Stderr, usage)
			return 2
		}
	} else if len(args) != 1 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	modes := map[string]string{"drain": "draining", "freeze": "frozen", "resume": "running"}
	if mode, ok := modes[args[0]]; ok {
		if err := maintenance.SetWithin(mode, wait); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	} else if args[0] != "status" {
		return 2
	}
	state, err := maintenance.Status()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if json.NewEncoder(os.Stdout).Encode(state) != nil {
		return 1
	}
	return 0
}
