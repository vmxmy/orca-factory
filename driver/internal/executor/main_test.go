package executor

import (
	"os"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/testenv"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	cleanup := testenv.IsolateStatus()
	goleak.VerifyTestMain(m, goleak.Cleanup(func(code int) {
		cleanup()
		os.Exit(code)
	}))
}
