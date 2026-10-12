package orca

import (
	"os"
	"testing"

	"github.com/vmxmy/orca-factory/driver/testsupport"
)

func TestMain(m *testing.M) {
	cleanup := testsupport.IsolateStatus()
	code := m.Run()
	cleanup()
	os.Exit(code)
}
