package config

import (
	"os"
	"testing"

	"github.com/vmxmy/orca-factory/driver/testsupport"
)

func TestMain(m *testing.M) {
	cleanup := testsupport.IsolateHome()
	code := m.Run()
	cleanup()
	os.Exit(code)
}
