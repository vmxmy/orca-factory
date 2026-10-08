package maintenance_test

import (
	"testing"

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
