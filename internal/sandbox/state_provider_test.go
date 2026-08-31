package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// stateProvider is an in-memory protocol double for SessionManager tests. It
// models resource identity and loss but never creates directories or processes.
// Real filesystem, command, and daemon recovery contracts run against Docker.
type stateProvider struct {
	mu    sync.Mutex
	boxes map[string]*stateSandbox
}

func newStateProvider() *stateProvider { return &stateProvider{boxes: map[string]*stateSandbox{}} }
func (*stateProvider) Name() string    { return "state-test" }
func (p *stateProvider) Create(ctx context.Context, key string, _ Spec) (Ref, Sandbox, error) {
	if err := ctx.Err(); err != nil {
		return Ref{}, nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	box := p.boxes[key]
	if box == nil || box.isDestroyed() {
		box = &stateSandbox{id: key, files: map[string][]byte{}}
		p.boxes[key] = box
	}
	return Ref{Provider: p.Name(), ID: key}, box, nil
}
func (p *stateProvider) Attach(ctx context.Context, key string, ref Ref, _ Spec) (Sandbox, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ref.Provider != p.Name() || ref.ID != key {
		return nil, Permanent(errors.New("wrong test resource owner"))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	box := p.boxes[ref.ID]
	if box == nil || box.isDestroyed() {
		return nil, ErrNotFound
	}
	return box, nil
}

type stateSandbox struct {
	mu        sync.Mutex
	id        string
	files     map[string][]byte
	destroyed bool
}

func (s *stateSandbox) isDestroyed() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.destroyed }
func (s *stateSandbox) Root() string      { return "/state/" + s.id }
func (*stateSandbox) Exec(context.Context, Command) (*Result, error) {
	panic("state-only test double cannot execute commands")
}
func (s *stateSandbox) WriteFile(ctx context.Context, name string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.destroyed {
		return ErrNotFound
	}
	s.files[name] = append([]byte(nil), data...)
	return nil
}
func (s *stateSandbox) ReadFile(ctx context.Context, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.destroyed {
		return nil, ErrNotFound
	}
	data, ok := s.files[name]
	if !ok {
		return nil, fmt.Errorf("no test file %q", name)
	}
	return append([]byte(nil), data...), nil
}
func (s *stateSandbox) Destroy(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.destroyed = true
	s.files = nil
	return nil
}
