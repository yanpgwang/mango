package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Only application data belongs here. Credentials stay in the environment.
// One process/operator owns this file; this is not a distributed business ledger.
type gateState struct {
	BaseURL       string              `json:"base_url"`
	EnvironmentID string              `json:"environment_id,omitempty"`
	AgentID       string              `json:"agent_id,omitempty"`
	SessionID     string              `json:"session_id,omitempty"`
	Creating      string              `json:"creating,omitempty"`
	Cleaning      bool                `json:"cleaning,omitempty"`
	Decisions     map[string]decision `json:"decisions"`
}

type decision struct {
	Recorded  bool   `json:"recorded"`
	ReceiptID string `json:"receipt_id"`
	Decision  string `json:"decision"`
	DecidedBy string `json:"decided_by"`
}

func checkStateFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return fmt.Errorf("state must be a regular private file (0600): %s", path)
	}
	return nil
}

func readState(path string) (gateState, error) {
	var state gateState
	if err := checkStateFile(path); err != nil {
		return state, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("read state: %w", err)
	}
	if state.BaseURL == "" || state.Decisions == nil {
		return state, errors.New("state is missing its server or decisions")
	}
	for id, result := range state.Decisions {
		if id == "" || !result.Recorded || (result.Decision != "approve" && result.Decision != "reject") ||
			(result.ReceiptID != "r01" && result.ReceiptID != "r02") || (result.DecidedBy != "human" && result.DecidedBy != "application") {
			return state, errors.New("state contains an invalid decision; inspect it before resuming")
		}
	}
	return state, nil
}

// writeState publishes complete JSON with a same-directory atomic rename. The
// initial link is exclusive, so start cannot overwrite an existing journal.
func writeState(path string, state gateState, create bool) error {
	if !create {
		if err := checkStateFile(path); err != nil {
			return err
		}
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".hitl-state-*") // mode 0600
	if err != nil {
		return err
	}
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	if err := json.NewEncoder(file).Encode(state); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if create {
		err = os.Link(file.Name(), path)
	} else {
		err = os.Rename(file.Name(), path)
	}
	if err != nil {
		return err
	}
	parent, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	return parent.Sync()
}
