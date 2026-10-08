package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/status"
)

func init() { Register("status", showStatus) }

func showStatus(args []string) int {
	asJSON := len(args) == 1 && args[0] == "--json"
	if len(args) != 0 && !asJSON {
		fmt.Fprintln(os.Stderr, "Usage: orca-factory-driver status [--json]")
		return 2
	}
	view := status.Read(time.Now(), 0)
	if asJSON {
		if err := json.NewEncoder(os.Stdout).Encode(view); err != nil {
			fmt.Fprintln(os.Stderr, "cannot output driver status")
			return 1
		}
	} else {
		fmt.Printf("Driver %s · tickets %d · global pause %t (%ds) · Linear requests %d complexity %d\n",
			view.State, len(view.Tickets), view.GlobalPause.Active, view.GlobalPause.DurationSeconds, view.Linear.Requests, view.Linear.Complexity)
		fmt.Printf("GitHub requests %d\n", view.GitHub.Requests)
		for _, t := range view.Tickets {
			if d := t.Deployment; d != nil {
				fmt.Printf("Deployment %s queue=%q sha=%s state=%s reason=%q\n", t.Identifier, d.QueueID, d.SHA, d.State, d.Reason)
			}
		}
	}
	return 0
}
