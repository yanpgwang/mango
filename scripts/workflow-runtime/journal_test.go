package workflowruntime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var errFenced = errors.New("fenced")
var errCrash = errors.New("injected_ack_loss")

// This deliberately small SQL fixture proves protocol properties against real
// transactions. It is not a proposed product schema or a replacement for the
// Session-first authorization/locking/outbox transitions in internal/pg.
const studySchema = `
CREATE TABLE study_runs (
 id text PRIMARY KEY, source text NOT NULL, args jsonb NOT NULL,
 epoch integer NOT NULL DEFAULT 0, version integer NOT NULL DEFAULT 0,
 inbox_version integer NOT NULL DEFAULT 0, state text NOT NULL DEFAULT 'running',
 wake boolean NOT NULL DEFAULT true
);
CREATE TABLE study_trace (
 run_id text REFERENCES study_runs, seq integer, body jsonb NOT NULL,
 PRIMARY KEY (run_id, seq)
);
CREATE TABLE study_invocations (
 run_id text REFERENCES study_runs, id integer, args jsonb NOT NULL,
 accepted boolean NOT NULL, dispatched boolean NOT NULL DEFAULT false,
 PRIMARY KEY (run_id, id)
);
CREATE TABLE study_receipts (
 run_id text, id integer, outcome jsonb NOT NULL, delivered boolean NOT NULL DEFAULT false,
 completion_order bigint GENERATED ALWAYS AS IDENTITY,
 PRIMARY KEY (run_id,id), FOREIGN KEY (run_id,id) REFERENCES study_invocations
);`

type journal struct {
	pool   *pgxpool.Pool
	ctx    context.Context
	id     string
	denied map[string]bool
}

func (j *journal) claim() (int, error) {
	var epoch int
	err := j.pool.QueryRow(j.ctx, `UPDATE study_runs SET epoch=epoch+1,wake=false WHERE id=$1 AND state='running' RETURNING epoch`, j.id).Scan(&epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errFenced
	}
	return epoch, err
}

func (j *journal) trace() ([]frame, error) {
	rows, err := j.pool.Query(j.ctx, `SELECT body FROM study_trace WHERE run_id=$1 ORDER BY seq`, j.id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var frames []frame
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		f, err := decodeFrame(data)
		if err != nil {
			return nil, err
		}
		frames = append(frames, f)
	}
	return frames, rows.Err()
}

func (j *journal) lock(tx pgx.Tx, epoch int) (int, error) {
	var current, version int
	var state string
	if err := tx.QueryRow(j.ctx, `SELECT epoch,version,state FROM study_runs WHERE id=$1 FOR UPDATE`, j.id).Scan(&current, &version, &state); err != nil {
		return 0, err
	}
	if epoch != current || state != "running" {
		return 0, errFenced
	}
	return version, nil
}

func (j *journal) append(epoch, position int, f frame) error {
	if _, err := decodeFrame(canonical(f)); err != nil {
		return err
	}
	tx, err := j.pool.Begin(j.ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(j.ctx) }()
	version, err := j.lock(tx, epoch)
	if err != nil {
		return err
	}
	if position != version || position >= 512 {
		return errors.New("trace_position")
	}
	switch f.Kind {
	case "invoke":
		var count int
		if err := tx.QueryRow(j.ctx, `SELECT count(*) FROM study_invocations WHERE run_id=$1`, j.id).Scan(&count); err != nil {
			return err
		}
		if f.ID != count+1 {
			return errors.New("invocation_order")
		}
		var args struct {
			Prompt  string `json:"prompt"`
			Options struct {
				Agent string `json:"agent"`
			} `json:"options"`
		}
		decodeErr := json.Unmarshal(f.Args, &args)
		accepted := decodeErr == nil && args.Prompt != "" && len(args.Prompt) <= 8192 && args.Options.Agent == "researcher" && !j.denied[args.Options.Agent]
		_, err = tx.Exec(j.ctx, `INSERT INTO study_invocations(run_id,id,args,accepted) VALUES($1,$2,$3,$4)`, j.id, f.ID, f.Args, accepted)
		if err != nil {
			return err
		}
		if !accepted {
			outcome := frame{Kind: "deliver", ID: f.ID, Error: &failure{Code: "admission_denied"}}
			if _, err := tx.Exec(j.ctx, `INSERT INTO study_receipts(run_id,id,outcome) VALUES($1,$2,$3)`, j.id, f.ID, canonical(outcome)); err != nil {
				return err
			}
		}
	case "deliver":
		var data []byte
		var delivered bool
		if err := tx.QueryRow(j.ctx, `SELECT outcome,delivered FROM study_receipts WHERE run_id=$1 AND id=$2`, j.id, f.ID).Scan(&data, &delivered); err != nil {
			return err
		}
		var stored frame
		if err := json.Unmarshal(data, &stored); err != nil {
			return err
		}
		if delivered || string(canonical(stored)) != string(canonical(f)) {
			return errors.New("delivery_mismatch")
		}
		_, err = tx.Exec(j.ctx, `UPDATE study_receipts SET delivered=true WHERE run_id=$1 AND id=$2`, j.id, f.ID)
	case "return":
		var open int
		if err := tx.QueryRow(j.ctx, `SELECT count(*) FROM study_invocations i LEFT JOIN study_receipts r USING(run_id,id) WHERE i.run_id=$1 AND (r.id IS NULL OR NOT r.delivered)`, j.id).Scan(&open); err != nil {
			return err
		}
		if open != 0 {
			return errors.New("outstanding_invocations")
		}
		_, err = tx.Exec(j.ctx, `UPDATE study_runs SET state='completed' WHERE id=$1`, j.id)
	case "failure":
		_, err = tx.Exec(j.ctx, `UPDATE study_runs SET state='failed' WHERE id=$1`, j.id)
	default:
		return errors.New("invalid_trace_kind")
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(j.ctx, `INSERT INTO study_trace(run_id,seq,body) VALUES($1,$2,$3)`, j.id, position+1, canonical(f)); err != nil {
		return err
	}
	if _, err := tx.Exec(j.ctx, `UPDATE study_runs SET version=version+1 WHERE id=$1`, j.id); err != nil {
		return err
	}
	return tx.Commit(j.ctx)
}

func (j *journal) dispatch(limit int) ([]int, error) {
	tx, err := j.pool.Begin(j.ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(j.ctx) }()
	var state string
	if err := tx.QueryRow(j.ctx, `SELECT state FROM study_runs WHERE id=$1 FOR UPDATE`, j.id).Scan(&state); err != nil {
		return nil, err
	}
	if state != "running" {
		return nil, errFenced
	}
	var active int
	if err := tx.QueryRow(j.ctx, `SELECT count(*) FROM study_invocations i LEFT JOIN study_receipts r USING(run_id,id) WHERE i.run_id=$1 AND dispatched AND r.id IS NULL`, j.id).Scan(&active); err != nil {
		return nil, err
	}
	rows, err := tx.Query(j.ctx, `SELECT id FROM study_invocations WHERE run_id=$1 AND accepted AND NOT dispatched ORDER BY id LIMIT $2`, j.id, max(0, limit-active))
	if err != nil {
		return nil, err
	}
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err := tx.Exec(j.ctx, `UPDATE study_invocations SET dispatched=true WHERE run_id=$1 AND id=$2`, j.id, id); err != nil {
			return nil, err
		}
	}
	return ids, tx.Commit(j.ctx)
}

func (j *journal) complete(f frame) error {
	if _, err := decodeFrame(canonical(f)); err != nil || f.Kind != "deliver" {
		return errors.New("invalid_receipt")
	}
	tx, err := j.pool.Begin(j.ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(j.ctx) }()
	var state string
	if err := tx.QueryRow(j.ctx, `SELECT state FROM study_runs WHERE id=$1 FOR UPDATE`, j.id).Scan(&state); err != nil {
		return err
	}
	if state != "running" && state != "paused" {
		return errFenced
	}
	var dispatched bool
	if err := tx.QueryRow(j.ctx, `SELECT dispatched FROM study_invocations WHERE run_id=$1 AND id=$2`, j.id, f.ID).Scan(&dispatched); err != nil {
		return err
	}
	if !dispatched {
		return errors.New("not_dispatched")
	}
	if _, err := tx.Exec(j.ctx, `INSERT INTO study_receipts(run_id,id,outcome) VALUES($1,$2,$3)`, j.id, f.ID, canonical(f)); err != nil {
		return err
	}
	if _, err := tx.Exec(j.ctx, `UPDATE study_runs SET inbox_version=inbox_version+1,wake=true WHERE id=$1`, j.id); err != nil {
		return err
	}
	return tx.Commit(j.ctx)
}

func (j *journal) nextDelivery(pending []int) (*frame, error) {
	var data []byte
	err := j.pool.QueryRow(j.ctx, `SELECT outcome FROM study_receipts WHERE run_id=$1 AND NOT delivered ORDER BY completion_order LIMIT 1`, j.id).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f frame
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	for _, id := range pending {
		if f.ID == id {
			return &f, nil
		}
	}
	return nil, errors.New("receipt_not_pending")
}

func (j *journal) wait(epoch, observedInbox int) error {
	tx, err := j.pool.Begin(j.ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(j.ctx) }()
	if _, err := j.lock(tx, epoch); err != nil {
		return err
	}
	_, err = tx.Exec(j.ctx, `UPDATE study_runs SET wake=(inbox_version<>$2 OR EXISTS(SELECT 1 FROM study_receipts WHERE run_id=$1 AND NOT delivered)) WHERE id=$1`, j.id, observedInbox)
	if err != nil {
		return err
	}
	return tx.Commit(j.ctx)
}

func (j *journal) count() (int, error) {
	var count int
	err := j.pool.QueryRow(j.ctx, `SELECT count(*) FROM study_invocations WHERE run_id=$1`, j.id).Scan(&count)
	return count, err
}

func (j *journal) newRun(source string, args any) error {
	if len(source) == 0 || len(source) > 65536 || !boundedJSON(canonical(args)) {
		return errors.New("program_input_limit")
	}
	_, err := j.pool.Exec(j.ctx, `INSERT INTO study_runs(id,source,args) VALUES($1,$2,$3)`, j.id, source, canonical(args))
	return err
}

func mismatch(position int) error { return fmt.Errorf("replay_mismatch_at_%d", position) }
