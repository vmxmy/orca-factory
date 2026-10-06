package status

import (
	"encoding/json"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestStatusFilePush(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("DRIVER_STATUS_FILE", "")
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	Push(now, func(v *View) { v.Tickets = []Ticket{{Identifier: "initial"}} })
	stop := make(chan struct{})
	readErr := make(chan error, 1)
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			data, err := os.ReadFile(PushPath())
			if err == nil {
				var v View
				err = json.Unmarshal(data, &v)
			}
			if err != nil {
				select {
				case readErr <- err:
				default:
				}
				return
			}
		}
	}()
	for i := 0; i < 100; i++ {
		Push(now, func(v *View) {
			v.Tickets = make([]Ticket, 100)
			for j := range v.Tickets {
				v.Tickets[j] = Ticket{Repo: "repo", Identifier: "ZII-891", Owner: "执行器", Phase: "gate"}
			}
		})
	}
	close(stop)
	readers.Wait()
	select {
	case err := <-readErr:
		t.Fatal(err)
	default:
	}
	data, err := os.ReadFile(PushPath())
	if err != nil {
		t.Fatal(err)
	}
	var view View
	if err := json.Unmarshal(data, &view); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(view, Read(now)) {
		t.Fatalf("file differs from status JSON: %+v", view)
	}
	if len(view.Tickets) != 100 {
		t.Fatalf("lost write: %+v", view)
	}
}
