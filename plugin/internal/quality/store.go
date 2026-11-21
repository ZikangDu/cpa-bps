package quality

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

const maxStateBytes = 16 << 20
const maxAccounts = 128
const maxResults = 1000

type accountState struct {
	AuthID        string    `json:"auth_id"`
	Config        Config    `json:"config"`
	NextDue       time.Time `json:"next_due,omitempty"`
	LastStarted   time.Time `json:"last_started,omitempty"`
	ReservedUntil time.Time `json:"reserved_until,omitempty"`
	Blocked       bool      `json:"blocked"`
	Revision      uint64    `json:"revision"`
}

type state struct {
	Version  int                     `json:"version"`
	Accounts map[string]accountState `json:"accounts"`
	Results  []Result                `json:"results"`
}

func readState(path string) (state, error) {
	result := state{Version: 1, Accounts: map[string]accountState{}, Results: []Result{}}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxStateBytes {
		return state{}, ErrUnavailable
	}
	file, err := os.Open(path)
	if err != nil {
		return state{}, ErrUnavailable
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxStateBytes+1))
	if err != nil || len(raw) > maxStateBytes {
		return state{}, ErrUnavailable
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&result) != nil || dec.Decode(new(any)) != io.EOF || result.Version != 1 || result.Accounts == nil || len(result.Accounts) > maxAccounts || len(result.Results) > maxResults {
		return state{}, ErrUnavailable
	}
	for index, account := range result.Accounts {
		if !validIndex(index) || account.AuthID == "" || len(account.AuthID) > 1024 || ValidateConfig(account.Config) != nil {
			return state{}, ErrUnavailable
		}
	}
	for _, result := range result.Results {
		if !validIndex(result.AuthIndex) || !ValidModel(result.RequestedModel) || !validStatus(result.Status) || len(result.Answer) > MaxAnswerBytes+3 {
			return state{}, ErrUnavailable
		}
	}
	return result, nil
}

func writeState(path string, value state) error {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) >= maxStateBytes {
		return ErrUnavailable
	}
	dir := filepath.Dir(path)
	if os.MkdirAll(dir, 0700) != nil {
		return ErrUnavailable
	}
	file, err := os.CreateTemp(dir, ".cpa-bps-quality-*")
	if err != nil {
		return ErrUnavailable
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(append(raw, '\n'))
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return ErrUnavailable
	}
	if os.Rename(file.Name(), path) != nil {
		return ErrUnavailable
	}
	// Sync the containing directory so the rename survives a host interruption.
	if directory, err := os.Open(dir); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}
