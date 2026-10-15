package maintenance_test

import (
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
)

func TestAdmissionSerializesDrain(t *testing.T) {
	t.Setenv("AGENT_DRIVER_STATE_ROOT", t.TempDir())
	release, err := maintenance.Admission()
	if err != nil {
		t.Fatal(err)
	}
	if err := maintenance.Set("draining"); err == nil {
		t.Fatal("drain raced admitted dispatch")
	}
	release()
	if err := maintenance.Set("draining"); err != nil {
		t.Fatal(err)
	}
	if _, err := maintenance.Admission(); err == nil {
		t.Fatal("drain admitted dispatch")
	}
}

func TestDrainPersistsAndFreezeExcludesWriters(t *testing.T) {
	t.Setenv("AGENT_DRIVER_STATE_ROOT", t.TempDir())
	if err := maintenance.Set("draining"); err != nil {
		t.Fatal(err)
	}
	if err := maintenance.Admit(); err == nil {
		t.Fatal("drain admitted new dispatch")
	}
	release, err := maintenance.Write()
	if err != nil {
		t.Fatal("drain blocked existing write", err)
	}
	if err := maintenance.Set("frozen"); err == nil {
		t.Fatal("freeze ignored active writer")
	}
	session, err := maintenance.Session()
	if err != nil {
		t.Fatal(err)
	}
	observed, err := maintenance.Status()
	if err != nil || !observed.WritersActive || !observed.SessionsActive || observed.Mode != "draining" {
		t.Fatalf("maintenance omitted active local guards: %+v %v", observed, err)
	}
	release()
	session()
	if err := maintenance.Set("frozen"); err != nil {
		t.Fatal(err)
	}
	state, err := maintenance.Read()
	if err != nil || state.Mode != "frozen" {
		t.Fatalf("state not durable: %+v %v", state, err)
	}
	if _, err := maintenance.Write(); err == nil {
		t.Fatal("frozen state allowed write")
	}
	if err := maintenance.Set("running"); err != nil {
		t.Fatal(err)
	}
	if err := maintenance.Admit(); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceDrainWaitsForWriters(t *testing.T) {
	t.Setenv("AGENT_DRIVER_STATE_ROOT", t.TempDir())
	writer, err := maintenance.Write()
	if err != nil {
		t.Fatal(err)
	}
	defer writer()

	admitted, err := maintenance.Admission()
	if err != nil {
		t.Fatal(err)
	}
	if err := maintenance.SetWithin("draining", 200*time.Millisecond); err == nil {
		t.Fatal("drain finished while admitted dispatch was in flight")
	}
	if state, err := maintenance.Read(); err != nil || state.Mode != "running" {
		t.Fatalf("timed-out drain changed state: %+v %v", state, err)
	}
	if err := maintenance.Admit(); err != nil {
		t.Fatal("timed-out drain kept refusing dispatch:", err)
	}
	admitted()

	admitted, err = maintenance.Admission()
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan time.Time, 1)
	refused := make(chan error, 1)
	go func() {
		time.Sleep(500 * time.Millisecond)
		late, err := maintenance.Admission()
		if err == nil {
			late()
		}
		refused <- err
		time.Sleep(500 * time.Millisecond)
		released <- time.Now()
		admitted()
	}()
	if err := maintenance.SetWithin("draining", 5*time.Second); err != nil {
		t.Fatal("drain did not wait for admitted dispatch:", err)
	}
	finished := time.Now()
	if err := <-refused; err == nil {
		t.Fatal("admission arriving during drain wait was accepted")
	}
	if finished.Before(<-released) {
		t.Fatal("drain finished before admitted dispatch released")
	}
	if state, err := maintenance.Read(); err != nil || state.Mode != "draining" {
		t.Fatalf("drain not persisted: %+v %v", state, err)
	}
	if err := maintenance.Admit(); err == nil {
		t.Fatal("drained state admitted new dispatch")
	}
	if _, err := maintenance.Admission(); err == nil {
		t.Fatal("drained state admitted new dispatch")
	}
}
