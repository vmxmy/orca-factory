package status

import (
	"os"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/testenv"
)

func TestMain(m *testing.M) {
	cleanup := testenv.IsolateStatus()
	code := m.Run()
	cleanup()
	os.Exit(code)
}
