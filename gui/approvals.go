package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/unreallabsai/unreal-agent/harness/operation"
)

const (
	permissionAsk  = "ask"
	permissionAuto = "auto"
)

type pendingApproval struct {
	ID      operation.ID `json:"id"`
	Command string       `json:"command"`
}

// approvalGate sits between a coordinator and its operation manager. In ask
// mode it holds new shell operations until the user approves them. To the
// async coordinator a held command is just a tool call that is still running,
// so the agent keeps working on anything else meanwhile.
type approvalGate struct {
	ctx     context.Context
	next    operation.Manager
	ask     func() bool
	changed func()
	updates chan operation.Operation

	mu      sync.Mutex
	pending []operation.Operation
}

var _ operation.Manager = (*approvalGate)(nil)

func newApprovalGate(ctx context.Context, next operation.Manager, ask func() bool, changed func()) *approvalGate {
	gate := &approvalGate{ctx: ctx, next: next, ask: ask, changed: changed, updates: make(chan operation.Operation)}
	go func() {
		for {
			select {
			case update := <-next.Updates():
				if !gate.send(update) {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return gate
}

func (gate *approvalGate) Updates() <-chan operation.Operation {
	return gate.updates
}

func (gate *approvalGate) Add(current operation.Operation) error {
	// Only commands that have not started need approval; restored running
	// commands resume as before.
	if current.Type != operation.TypeShell || current.Status != operation.StatusReady || !gate.ask() {
		return gate.next.Add(current)
	}
	gate.mu.Lock()
	gate.pending = append(gate.pending, current)
	gate.mu.Unlock()
	gate.changed()
	return nil
}

func (gate *approvalGate) Cancel(id operation.ID, reason string) error {
	if held, ok := gate.take(id); ok {
		gate.changed()
		return gate.finish(held, operation.StatusCanceled, reason)
	}
	return gate.next.Cancel(id, reason)
}

func (gate *approvalGate) list() []pendingApproval {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	list := make([]pendingApproval, 0, len(gate.pending))
	for _, held := range gate.pending {
		command := ""
		if state, err := operation.DecodeShellState(held); err == nil {
			command = state.Input.Command
		}
		list = append(list, pendingApproval{ID: held.ID, Command: command})
	}
	return list
}

// decide approves or declines held commands; an empty id applies to all.
func (gate *approvalGate) decide(id operation.ID, approve bool) error {
	var chosen []operation.Operation
	gate.mu.Lock()
	gate.pending = slices.DeleteFunc(gate.pending, func(held operation.Operation) bool {
		if id == "" || held.ID == id {
			chosen = append(chosen, held)
			return true
		}
		return false
	})
	gate.mu.Unlock()
	if len(chosen) == 0 {
		return fmt.Errorf("no command %q is waiting for approval", id)
	}
	gate.changed()
	for _, held := range chosen {
		var err error
		if approve {
			err = gate.next.Add(held)
		} else {
			err = gate.finish(held, operation.StatusFailed, "The user declined to run this command. Ask what they would prefer, or try a different approach.")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (gate *approvalGate) take(id operation.ID) (operation.Operation, bool) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	index := slices.IndexFunc(gate.pending, func(held operation.Operation) bool { return held.ID == id })
	if index < 0 {
		return operation.Operation{}, false
	}
	held := gate.pending[index]
	gate.pending = slices.Delete(gate.pending, index, index+1)
	return held, true
}

// finish reports a held command as ended without running it, in the same
// terminal shape the shell operation itself produces.
func (gate *approvalGate) finish(held operation.Operation, status operation.Status, reason string) error {
	state, err := operation.DecodeShellState(held)
	if err != nil {
		return err
	}
	state.Phase = ""
	state.TerminalError = strings.TrimSpace(reason)
	state.ErrorTruncated = false
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	held.Status, held.State = status, encoded
	// The coordinator may be the caller (Cancel during a stop), so deliver
	// without waiting for it to read.
	go gate.send(held)
	return nil
}

func (gate *approvalGate) send(update operation.Operation) bool {
	select {
	case gate.updates <- update:
		return true
	case <-gate.ctx.Done():
		return false
	}
}
