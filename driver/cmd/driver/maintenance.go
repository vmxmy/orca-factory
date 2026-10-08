package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"

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
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: driver maintenance status|drain|freeze|resume")
		return 2
	}
	modes := map[string]string{"drain": "draining", "freeze": "frozen", "resume": "running"}
	if mode, ok := modes[args[0]]; ok {
		if err := maintenance.Set(mode); err != nil {
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
