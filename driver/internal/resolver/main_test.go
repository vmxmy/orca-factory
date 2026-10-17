package resolver

import (
	"os"
	"testing"

	"github.com/vmxmy/orca-factory/driver/testsupport"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	cleanup := testsupport.IsolateStatus()
	goleak.VerifyTestMain(m, goleak.Cleanup(func(code int) {
		cleanup()
		os.Exit(code)
	}))
}
