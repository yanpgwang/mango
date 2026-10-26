package temporal

// Test-only bridge to the existing isolated QuickJS study profile. No default
// worker, application, API or release artifact imports this adapter.
import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg"
	"go.temporal.io/sdk/temporal"
)

const quickJSFrameLimit = 64 << 10

type quickJSFailure struct {
	Code string `json:"code"`
}
type quickJSFrame struct {
	Kind    string          `json:"kind"`
	ID      int             `json:"id,omitempty"`
	Args    json.RawMessage `json:"args,omitempty"`
	Value   json.RawMessage `json:"value,omitempty"`
	Error   *quickJSFailure `json:"error,omitempty"`
	Code    string          `json:"code,omitempty"`
	Pending []int           `json:"pending,omitempty"`
}

type quickJSProcess struct {
	name   string
	cmd    *exec.Cmd
	input  io.WriteCloser
	output *bufio.Scanner
	cancel context.CancelFunc
}
type quickJSExit struct {
	Name          string
	Code          int
	OOM, Observed bool
}

func quickJSDocker(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	err := cmd.Run()
	return out.Bytes(), err
}
func startQuickJSProcess(ctx context.Context, image, source string, args json.RawMessage) (*quickJSProcess, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	p := &quickJSProcess{name: "mango-quickjs-vertical-" + domain.NewRandomIDGen().NewID(""), cancel: cancel}
	p.cmd = exec.CommandContext(ctx, "docker", "run", "--interactive", "--name", p.name,
		"--network=none", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--pids-limit=32", "--memory=128m", "--memory-swap=128m", "--cpus=1", "--ipc=none",
		"--ulimit", "cpu=3:3", "--ulimit", "stack=2097152:2097152", "--ulimit", "core=0:0", "--ulimit", "nofile=32:32", "--user=65532:65532", image)
	var err error
	p.input, err = p.cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	output, err := p.cmd.StdoutPipe()
	if err != nil {
		_ = p.input.Close()
		cancel()
		return nil, err
	}
	p.cmd.Stderr = io.Discard
	p.output = bufio.NewScanner(output)
	p.output.Buffer(make([]byte, 4096), quickJSFrameLimit+1)
	if err = p.cmd.Start(); err != nil {
		_ = p.input.Close()
		cancel()
		return nil, err
	}
	if err = p.send(map[string]any{"kind": "start", "source": source, "args": args}); err != nil {
		_, _ = p.close()
		return nil, err
	}
	return p, nil
}
func (p *quickJSProcess) send(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > 131072 {
		return errors.New("bounded QuickJS input exceeded")
	}
	_, err = fmt.Fprintln(p.input, string(data))
	return err
}
func (p *quickJSProcess) close() (quickJSExit, error) {
	_ = p.input.Close()
	cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = p.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		_, _ = quickJSDocker(cleanup, "kill", p.name)
		<-done
	}
	state, err := quickJSDocker(cleanup, "inspect", "--format", "{{.State.ExitCode}} {{.State.OOMKilled}} {{.State.Running}}", p.name)
	result := quickJSExit{Name: p.name}
	var running bool
	if err == nil {
		_, err = fmt.Sscanf(string(state), "%d %t %t", &result.Code, &result.OOM, &running)
		result.Observed = err == nil && !running
	}
	_, removeErr := quickJSDocker(cleanup, "rm", "--force", p.name)
	p.cancel()
	return result, errors.Join(err, removeErr)
}

func decodeQuickJSFrame(data []byte) (quickJSFrame, error) {
	var f quickJSFrame
	if len(data) > quickJSFrameLimit {
		return f, errors.New("frame limit")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&f); err != nil {
		return f, errors.New("invalid frame")
	}
	if d.Decode(new(any)) != io.EOF {
		return f, errors.New("trailing frame data")
	}
	switch f.Kind {
	case "invoke":
		if f.ID < 1 || f.ID > 128 || len(f.Args) == 0 || len(f.Value) > 0 || f.Error != nil || f.Code != "" || len(f.Pending) > 0 {
			return f, errors.New("invalid invocation frame")
		}
	case "wait":
		if len(f.Pending) == 0 || len(f.Pending) > 128 || f.ID != 0 || len(f.Args) > 0 || len(f.Value) > 0 || f.Error != nil || f.Code != "" {
			return f, errors.New("invalid wait frame")
		}
		seen := map[int]bool{}
		for _, id := range f.Pending {
			if id < 1 || id > 128 || seen[id] {
				return f, errors.New("invalid pending IDs")
			}
			seen[id] = true
		}
	case "return":
		if len(f.Value) == 0 || len(f.Value) > 32768 || f.ID != 0 || len(f.Args) > 0 || f.Error != nil || f.Code != "" || len(f.Pending) > 0 {
			return f, errors.New("invalid return frame")
		}
	case "failure":
		return f, errors.New("QuickJS engine/program failure")
	default:
		return f, errors.New("unknown frame")
	}
	return f, nil
}

// Read the entire bounded quantum before proposing ANY effect. In particular,
// an emitted invoke followed by a resource/protocol failure is never admitted.
func (p *quickJSProcess) quantum() ([]quickJSFrame, quickJSFrame, error) {
	frames := []quickJSFrame{}
	for range 129 {
		if !p.output.Scan() {
			return nil, quickJSFrame{}, errors.New("QuickJS ended or exceeded frame limit")
		}
		f, err := decodeQuickJSFrame(p.output.Bytes())
		if err != nil {
			return nil, f, err
		}
		if f.Kind != "invoke" {
			return frames, f, nil
		}
		frames = append(frames, f)
	}
	return nil, quickJSFrame{}, errors.New("QuickJS quantum frame limit")
}

type quickJSCall struct {
	id        int
	cursor    int64
	entry     pg.WorkflowPumpEntry
	delivered bool
}
type quickJSTestEvaluator struct {
	image, profile    string
	killAtCursor      int
	killEnabled       bool
	replaySourceFault string
	killed            atomic.Bool
	mu                sync.Mutex
	exits             []quickJSExit
	evaluated         []int
}

func quickJSFatal() error {
	return temporal.NewNonRetryableApplicationError("QuickJS candidate requires reconciliation", "QuickJSStudyFailure", nil)
}
func (e *quickJSTestEvaluator) Next(ctx context.Context, in pg.WorkflowPumpSnapshot) (proposal *pg.WorkflowPumpCommand, err error) {
	if in.Request.RuntimeProfile != e.profile {
		return nil, quickJSFatal()
	}
	if n := len(in.Journal); n > 0 && in.Journal[n-1].Decision.Type == "return" {
		return nil, nil
	}
	source := in.Request.Source
	if e.replaySourceFault != "" && len(in.Journal) > 0 {
		source = e.replaySourceFault
	} // Deliberately corrupt replay in one negative fixture only.
	p, err := startQuickJSProcess(ctx, e.image, source, in.Request.Input)
	if err != nil {
		return nil, quickJSFatal()
	}
	defer func() {
		exit, closeErr := p.close()
		e.mu.Lock()
		e.exits = append(e.exits, exit)
		e.evaluated = append(e.evaluated, len(in.Journal))
		e.mu.Unlock()
		if closeErr != nil && err == nil {
			proposal = nil
			err = quickJSFatal()
		}
	}()
	pos, nextID := 0, 1
	calls := map[int]*quickJSCall{}
	byCursor := map[int64]*quickJSCall{}
	for range 257 {
		frames, tail, readErr := p.quantum()
		if readErr != nil {
			return nil, quickJSFatal()
		}
		var firstNew *pg.WorkflowPumpCommand
		for _, f := range frames {
			if f.ID != nextID {
				return nil, quickJSFatal()
			}
			nextID++
			var args struct {
				Prompt  string `json:"prompt"`
				Options struct {
					Agent string `json:"agent"`
				} `json:"options"`
			}
			d := json.NewDecoder(bytes.NewReader(f.Args))
			d.DisallowUnknownFields()
			if d.Decode(&args) != nil || d.Decode(new(any)) != io.EOF {
				return nil, quickJSFatal()
			}
			command := pg.WorkflowPumpCommand{Kind: "invoke", AgentName: args.Options.Agent, Prompt: args.Prompt}
			call := &quickJSCall{id: f.ID, cursor: -1}
			calls[f.ID] = call
			if pos < len(in.Journal) {
				entry := in.Journal[pos]
				if !reflect.DeepEqual(entry.Command, command) || (entry.Decision.Type != "invocation" && entry.Decision.Type != "rejected") {
					return nil, quickJSFatal()
				}
				call.cursor = int64(pos)
				call.entry = entry
				byCursor[int64(pos)] = call
				pos++
			} else if firstNew == nil {
				firstNew = &command
			}
		}
		if tail.Kind == "wait" {
			pending := map[int]bool{}
			for _, id := range tail.Pending {
				call := calls[id]
				if call == nil || call.delivered {
					return nil, quickJSFatal()
				}
				pending[id] = true
			}
			for id, call := range calls {
				if !call.delivered && !pending[id] {
					return nil, quickJSFatal()
				}
			}
		} else {
			for _, call := range calls {
				if !call.delivered {
					return nil, quickJSFatal()
				}
			}
		}
		if firstNew != nil {
			if pos != len(in.Journal) || tail.Kind != "wait" {
				return nil, quickJSFatal()
			}
			if e.killEnabled && len(in.Journal) == e.killAtCursor && e.killed.CompareAndSwap(false, true) {
				_, _ = quickJSDocker(ctx, "kill", p.name)
				return nil, errors.New("injected evaluator loss before frontier commit")
			}
			return firstNew, nil
		}
		if pos < len(in.Journal) {
			entry := in.Journal[pos]
			if entry.Decision.Type != "delivery" || entry.Command.Kind != "deliver" || entry.Decision.Delivery == nil || tail.Kind != "wait" {
				return nil, quickJSFatal()
			}
			delivery := entry.Decision.Delivery
			call := byCursor[delivery.InvocationCursor]
			if call == nil || call.delivered || entry.Command.InvocationCursor != delivery.InvocationCursor {
				return nil, quickJSFatal()
			}
			receipt := delivery.Receipt
			if receipt.AdapterVersion != 1 {
				return nil, quickJSFatal()
			}
			frame := quickJSFrame{Kind: "deliver", ID: call.id}
			switch receipt.Type {
			case "text":
				frame.Value, _ = json.Marshal(receipt.Text)
			case "failure":
				if receipt.ErrorCode == "" {
					return nil, quickJSFatal()
				}
				frame.Error = &quickJSFailure{Code: receipt.ErrorCode}
			default:
				return nil, quickJSFatal()
			}
			if p.send(frame) != nil {
				return nil, quickJSFatal()
			}
			call.delivered = true
			pos++
			continue // C drains microtasks after THIS delivery before the next read.
		}
		if tail.Kind == "return" {
			return &pg.WorkflowPumpCommand{Kind: "return", Value: tail.Value}, nil
		}
		for _, id := range tail.Pending {
			call := calls[id]
			if call.cursor < 0 {
				return nil, quickJSFatal()
			}
			if call.entry.Receipt != nil || call.entry.Decision.Type == "rejected" {
				return &pg.WorkflowPumpCommand{Kind: "deliver", InvocationCursor: call.cursor}, nil
			}
		}
		return nil, nil
	}
	return nil, quickJSFatal()
}
