package workflowruntime_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

func unique() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func docker(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var output bytes.Buffer
	cmd.Stdout = &output
	// Private engine/build errors may contain source. Test diagnostics use IDs
	// and reason codes instead of dumping docker stdout/stderr on failure.
	cmd.Stderr = io.Discard
	err := cmd.Run()
	return output.Bytes(), err
}

func database(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := t.Context()
	name := "mango-workflow-study-pg-" + unique()
	_, err := docker(ctx, "run", "-d", "--rm", "--name", name, "--publish", "127.0.0.1::5432", "--env", "POSTGRES_HOST_AUTH_METHOD=trust", "postgres:17.5-alpine")
	require.NoError(t, err, "start owned PostgreSQL fixture")
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := docker(cleanup, "rm", "--force", name)
		require.NoError(t, err, "remove owned PostgreSQL fixture")
	})
	port, err := docker(ctx, "port", name, "5432/tcp")
	require.NoError(t, err)
	var pool *pgxpool.Pool
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		pool, err = pgxpool.New(ctx, "postgres://postgres@"+strings.TrimSpace(string(port))+"/postgres?sslmode=disable")
		if err == nil {
			err = pool.Ping(ctx)
			if err == nil {
				break
			}
			pool.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.NoError(t, err, "connect owned PostgreSQL fixture")
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, studySchema)
	require.NoError(t, err)
	return pool
}

type evaluator struct {
	cancel context.CancelFunc
	cmd    *exec.Cmd
	input  io.WriteCloser
	output *bufio.Scanner
	name   string
}

type engineExit struct {
	Code      int
	OOMKilled bool
	Observed  bool
}

func startEvaluator(ctx context.Context, image, source string, args json.RawMessage) (*evaluator, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	e := &evaluator{cancel: cancel, name: "mango-workflow-study-js-" + unique()}
	e.cmd = exec.CommandContext(ctx, "docker", "run", "--interactive", "--name", e.name,
		"--network=none", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--pids-limit=32", "--memory=128m", "--memory-swap=128m", "--cpus=1", "--ipc=none",
		"--ulimit", "cpu=3:3", "--ulimit", "stack=2097152:2097152", "--ulimit", "core=0:0", "--ulimit", "nofile=32:32", "--user=65532:65532", image)
	var err error
	e.input, err = e.cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	output, err := e.cmd.StdoutPipe()
	if err != nil {
		_ = e.input.Close()
		cancel()
		return nil, err
	}
	e.cmd.Stderr = io.Discard
	e.output = bufio.NewScanner(output)
	e.output.Buffer(make([]byte, 4096), maxFrame+1)
	if err := e.cmd.Start(); err != nil {
		_ = e.input.Close()
		cancel()
		return nil, err
	}
	data := canonical(map[string]any{"kind": "start", "source": source, "args": args})
	if _, err := fmt.Fprintln(e.input, string(data)); err != nil {
		e.close()
		return nil, err
	}
	return e, nil
}

func (e *evaluator) send(f frame) error {
	_, err := fmt.Fprintln(e.input, string(canonical(f)))
	return err
}

func (e *evaluator) read() (frame, error) {
	if !e.output.Scan() {
		return frame{}, errors.New("engine_ended_or_frame_limit")
	}
	return decodeFrame(e.output.Bytes())
}

func (e *evaluator) close() (engineExit, error) {
	_ = e.input.Close()
	cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = e.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		_, _ = docker(cleanup, "kill", e.name)
		<-done
	}
	var code int
	var oom, running bool
	state, err := docker(cleanup, "inspect", "--format", "{{.State.ExitCode}} {{.State.OOMKilled}} {{.State.Running}}", e.name)
	observed := false
	if err == nil {
		_, err = fmt.Sscanf(string(state), "%d %t %t", &code, &oom, &running)
		observed = err == nil && !running
	}
	// Explicit cleanup also owns containers surviving a killed docker client.
	_, cleanupErr := docker(cleanup, "rm", "--force", e.name)
	e.cancel()
	return engineExit{Code: code, OOMKilled: oom, Observed: observed}, cleanupErr
}

type pumpOptions struct {
	crashAfter  int
	sourceFault string
	beforeWait  func()
	diagnostics *engineExit
}

func pump(j *journal, image string, options pumpOptions) (result frame, pumpErr error) {
	history, err := j.trace()
	if err != nil {
		return frame{}, err
	}
	if len(history) > 0 {
		last := history[len(history)-1]
		if last.Kind == "return" || last.Kind == "failure" {
			return last, nil // A final SQL commit whose Activity reply was lost.
		}
	}
	epoch, err := j.claim()
	if err != nil {
		return frame{}, err
	}
	var source string
	var args []byte
	if err := j.pool.QueryRow(j.ctx, `SELECT source,args FROM study_runs WHERE id=$1`, j.id).Scan(&source, &args); err != nil {
		return frame{}, err
	}
	if options.sourceFault != "" {
		source = options.sourceFault // Deliberately broken evaluator/profile fixture.
	}
	e, err := startEvaluator(j.ctx, image, source, args)
	if err != nil {
		return frame{}, err
	}
	defer func() {
		exit, cleanupErr := e.close()
		if cleanupErr != nil {
			pumpErr = errors.New("owned_engine_cleanup")
		}
		if options.diagnostics != nil {
			*options.diagnostics = exit
		}
	}()
	position := 0
	appendFrame := func(f frame) error {
		if position < len(history) {
			if !bytes.Equal(canonical(history[position]), canonical(f)) {
				return mismatch(position)
			}
		} else {
			if err := j.append(epoch, position, f); err != nil {
				return err
			}
			history = append(history, f)
		}
		position++
		if options.crashAfter == position {
			return errCrash
		}
		return nil
	}
	for {
		f, err := e.read()
		if err != nil {
			return frame{}, err
		}
		if f.Kind != "wait" {
			if f.Kind != "invoke" && f.Kind != "return" && f.Kind != "failure" {
				return frame{}, errors.New("unexpected_engine_frame")
			}
			if err := appendFrame(f); err != nil {
				return frame{}, err
			}
			if f.Kind != "invoke" {
				return f, nil
			}
			continue
		}
		var delivery *frame
		if position < len(history) {
			if history[position].Kind != "deliver" {
				return frame{}, mismatch(position)
			}
			candidate := history[position]
			found := false
			for _, id := range f.Pending {
				found = found || id == candidate.ID
			}
			if !found {
				return frame{}, mismatch(position)
			}
			delivery = &candidate
		} else {
			var observedInbox int
			if err := j.pool.QueryRow(j.ctx, `SELECT inbox_version FROM study_runs WHERE id=$1`, j.id).Scan(&observedInbox); err != nil {
				return frame{}, err
			}
			delivery, err = j.nextDelivery(f.Pending)
			if err != nil {
				return frame{}, err
			}
			if delivery == nil {
				if options.beforeWait != nil {
					options.beforeWait()
				}
				return f, j.wait(epoch, observedInbox)
			}
		}
		// A success or rejection is committed before JS can observe it. Replay
		// delivers only the recorded next item, never all cached results at once.
		if err := appendFrame(*delivery); err != nil {
			return frame{}, err
		}
		if err := e.send(*delivery); err != nil {
			return frame{}, err
		}
	}
}

type study struct {
	pool  *pgxpool.Pool
	image string
}

func (s study) run(t *testing.T, source string, args any) *journal {
	t.Helper()
	j := &journal{pool: s.pool, ctx: t.Context(), id: unique(), denied: map[string]bool{}}
	require.NoError(t, j.newRun(source, args))
	return j
}

func (s study) step(t *testing.T, j *journal, want string) frame {
	t.Helper()
	result, err := pump(j, s.image, pumpOptions{})
	require.NoError(t, err)
	require.Equal(t, want, result.Kind)
	return result
}

func settle(t *testing.T, j *journal, id int, value any) {
	t.Helper()
	require.NoError(t, j.complete(frame{Kind: "deliver", ID: id, Value: canonical(value)}))
}

func dispatch(t *testing.T, j *journal, limit int, want ...int) {
	t.Helper()
	ids, err := j.dispatch(limit)
	require.NoError(t, err)
	require.Equal(t, want, ids)
}

func TestEvaluatorStudy(t *testing.T) {
	if os.Getenv("MANGO_TEST_WORKFLOW_RUNTIME") != "1" {
		t.Skip("explicit isolated-engine study; use make test-workflow-runtime")
	}
	pool := database(t)
	for _, engine := range []string{"quickjs", "node"} {
		t.Run(engine, func(t *testing.T) {
			identity, err := docker(t.Context(), "image", "inspect", "mango-workflow-study:"+engine, "--format", "{{.Id}} {{.Os}}/{{.Architecture}}")
			require.NoError(t, err, "build research images first")
			fields := strings.Fields(string(identity))
			require.Len(t, fields, 2)
			t.Logf("engine=%s image=%s platform=%s", engine, fields[0], fields[1])
			s := study{pool: pool, image: fields[0]}
			t.Run("sequential_and_repeated_calls", s.sequential)
			t.Run("ordered_deliveries_and_callbacks", s.ordering)
			t.Run("acknowledgement_loss", s.acknowledgementLoss)
			t.Run("rejected_admission_and_changed_policy", s.rejectedAdmission)
			t.Run("failure_keeps_sibling_and_allows_recovery", s.partialFailure)
			t.Run("deferred_rejection", s.deferredRejection)
			t.Run("bounded_result_driven_loop", s.loop)
			t.Run("deferred_concurrency", s.capacity)
			t.Run("wait_handoff", s.waitHandoff)
			t.Run("stale_epoch_pause_and_stop", s.fencing)
			t.Run("trace_mismatch", s.traceMismatch)
			t.Run("restricted_profile", s.profile)
			t.Run("resource_bounds", s.resources)
			t.Run("bounded_replay_measurement", s.measure)
		})
	}
}
