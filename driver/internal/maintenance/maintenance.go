package maintenance

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type State struct {
	Mode      string    `json:"mode"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func root() (string, error) {
	if root := os.Getenv("AGENT_DRIVER_STATE_ROOT"); root != "" {
		return root, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "orca-factory"), nil
}
func lock(name string, mode int) (*os.File, error) {
	root, err := root()
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(root, "maintenance-"+name+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(file.Fd()), mode|syscall.LOCK_NB); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}
func Read() (State, error) {
	root, err := root()
	if err != nil {
		return State{}, err
	}
	raw, err := os.ReadFile(filepath.Join(root, "maintenance.json"))
	if errors.Is(err, os.ErrNotExist) {
		return State{Mode: "running"}, nil
	}
	if err != nil {
		return State{}, err
	}
	var state State
	if err = json.Unmarshal(raw, &state); err != nil {
		return State{}, err
	}
	if state.Mode != "running" && state.Mode != "draining" && state.Mode != "frozen" {
		return State{}, errors.New("invalid maintenance state")
	}
	return state, nil
}

type Observation struct {
	State
	WritersActive  bool `json:"writersActive"`
	SessionsActive bool `json:"sessionsActive"`
}

func Status() (Observation, error) {
	state, err := Read()
	if err != nil {
		return Observation{}, err
	}
	active := func(name string) (bool, error) {
		file, err := lock(name, syscall.LOCK_EX)
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return false, file.Close()
	}
	writers, err := active("writes")
	if err != nil {
		return Observation{}, err
	}
	sessions, err := active("sessions")
	return Observation{State: state, WritersActive: writers, SessionsActive: sessions}, err
}

func Set(mode string) error { return SetWithin(mode, 0) }

// SetWithin holds the intent lock while it waits, so a new Admission is
// refused and in-flight admissions cannot starve the change. Plain external
// writes never delay drain or resume; only freeze excludes them.
func SetWithin(mode string, wait time.Duration) (err error) {
	if mode != "running" && mode != "draining" && mode != "frozen" {
		return errors.New("invalid maintenance mode")
	}
	deadline := time.Now().Add(wait)
	intent, err := waitLock("intent", deadline)
	if err != nil {
		return fmt.Errorf("another maintenance change is pending: %w", err)
	}
	defer func() { err = errors.Join(err, intent.Close()) }()
	admissions, err := waitLock("admissions", deadline)
	if err != nil {
		return fmt.Errorf("admitted claim or dispatch still holds the maintenance lock: %w", err)
	}
	defer func() { err = errors.Join(err, admissions.Close()) }()
	old, err := Read()
	if err != nil {
		return err
	}
	if mode == "frozen" && old.Mode != "draining" {
		return errors.New("drain required before freeze")
	}
	if mode == "frozen" {
		writes, lockErr := lock("writes", syscall.LOCK_EX)
		if lockErr != nil {
			return lockErr
		}
		defer func() { err = errors.Join(err, writes.Close()) }()
		sessions, lockErr := lock("sessions", syscall.LOCK_EX)
		if lockErr != nil {
			return errors.New("active executor or resolver; keep draining")
		}
		defer func() { err = errors.Join(err, sessions.Close()) }()
	}
	root, err := root()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(State{Mode: mode, UpdatedAt: time.Now().UTC()})
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(root, "maintenance-*.json")
	if err != nil {
		return err
	}
	name := file.Name()
	defer func() {
		removeErr := os.Remove(name)
		if !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(raw)
	}
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(root, "maintenance.json"))
}
func waitLock(name string, deadline time.Time) (*os.File, error) {
	for {
		file, err := lock(name, syscall.LOCK_EX)
		if !errors.Is(err, syscall.EWOULDBLOCK) || !time.Now().Before(deadline) {
			return file, err
		}
		time.Sleep(min(50*time.Millisecond, time.Until(deadline)))
	}
}
func Admission() (func(), error) {
	release, err := Write()
	if err != nil {
		return nil, err
	}
	admissions, err := lock("admissions", syscall.LOCK_SH)
	if err != nil {
		release()
		return nil, err
	}
	if err = Admit(); err != nil {
		err = errors.Join(err, admissions.Close())
		release()
		return nil, err
	}
	return func() {
		if err := admissions.Close(); err != nil {
			log.Print("cannot close maintenance guard")
		}
		release()
	}, nil
}

// ErrRefused marks a claim or dispatch the gate refused before it reached any
// external system, so its effects are known not to exist.
var ErrRefused = errors.New("maintenance refused new claim or dispatch")

func Admit() error {
	intent, err := lock("intent", syscall.LOCK_SH)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return fmt.Errorf("%w: %s", ErrRefused, "maintenance change pending; new claim or dispatch refused")
	}
	if err != nil {
		return err
	}
	if err = intent.Close(); err != nil {
		return err
	}
	state, err := Read()
	if err != nil {
		return err
	}
	if state.Mode != "running" {
		return fmt.Errorf("%w: %s", ErrRefused, "maintenance blocks new claim or dispatch")
	}
	return nil
}
func Write() (func(), error) {
	file, err := lock("writes", syscall.LOCK_SH)
	if err != nil {
		return nil, err
	}
	state, err := Read()
	if err != nil || state.Mode == "frozen" {
		closeErr := file.Close()
		if err == nil {
			err = errors.New("maintenance freezes external writes")
		}
		return nil, errors.Join(err, closeErr)
	}
	return func() {
		if err := file.Close(); err != nil {
			log.Print("cannot close maintenance guard")
		}
	}, nil
}
func Session() (func(), error) {
	guard, err := Write()
	if err != nil {
		return nil, err
	}
	defer guard()
	file, err := lock("sessions", syscall.LOCK_SH)
	if err != nil {
		return nil, err
	}
	return func() {
		if err := file.Close(); err != nil {
			log.Print("cannot close maintenance guard")
		}
	}, nil
}
