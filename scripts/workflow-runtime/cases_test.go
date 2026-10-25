package workflowruntime_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const researcher = `{agent:"researcher"}`

func (s study) sequential(t *testing.T) {
	j := s.run(t, `const a=await agent("same",`+researcher+`); const b=await agent("same",`+researcher+`); return {a,b};`, nil)
	s.step(t, j, "wait")
	dispatch(t, j, 4, 1)
	settle(t, j, 1, "first")
	s.step(t, j, "wait")
	dispatch(t, j, 4, 2)
	settle(t, j, 2, "second")
	result := s.step(t, j, "return")
	require.JSONEq(t, `{"a":"first","b":"second"}`, string(result.Value))
	count, err := j.count()
	require.NoError(t, err)
	require.Equal(t, 2, count, "identical calls are distinct work")
	require.Equal(t, canonical(result), canonical(s.step(t, j, "return")))
}

func (s study) ordering(t *testing.T) {
	source := `const a=agent("A",` + researcher + `).then(x=>agent("C:"+x,` + researcher + `));
const b=agent("B",` + researcher + `).then(x=>agent("D:"+x,` + researcher + `));
return await Promise.all([a,b]);`
	j := s.run(t, source, nil)
	s.step(t, j, "wait")
	dispatch(t, j, 4, 1, 2)
	settle(t, j, 2, "b")
	s.step(t, j, "wait")
	dispatch(t, j, 4, 3)
	settle(t, j, 1, "a")
	s.step(t, j, "wait")
	dispatch(t, j, 4, 4)
	// On restart both early results are cached. Their original delivery order
	// must still produce invocation 3=D:b and invocation 4=C:a.
	s.step(t, j, "wait")
	var third, fourth string
	require.NoError(t, j.pool.QueryRow(j.ctx, `SELECT args->>'prompt' FROM study_invocations WHERE run_id=$1 AND id=3`, j.id).Scan(&third))
	require.NoError(t, j.pool.QueryRow(j.ctx, `SELECT args->>'prompt' FROM study_invocations WHERE run_id=$1 AND id=4`, j.id).Scan(&fourth))
	require.Equal(t, "D:b", third)
	require.Equal(t, "C:a", fourth)
	settle(t, j, 3, "D")
	settle(t, j, 4, "C")
	result := s.step(t, j, "return")
	require.JSONEq(t, `["C","D"]`, string(result.Value), "Promise.all keeps input order")
	history, err := j.trace()
	require.NoError(t, err)
	var delivered []int
	for _, f := range history {
		if f.Kind == "deliver" {
			delivered = append(delivered, f.ID)
		}
	}
	require.Equal(t, []int{2, 1, 3, 4}, delivered)
}

func (s study) acknowledgementLoss(t *testing.T) {
	source := `const a=agent("a",` + researcher + `); const b=agent("b",` + researcher + `).then(x=>agent(x+"c",` + researcher + `)); return await Promise.all([a,b]);`
	// Independently specified trace: invoke A, invoke B, deliver B, invoke C,
	// deliver A, deliver C, return. Replace the real process after every commit.
	for boundary := 1; boundary <= 7; boundary++ {
		t.Run(fmt.Sprint(boundary), func(t *testing.T) {
			j := s.run(t, source, nil)
			crashed := false
			step := func(want string) {
				t.Helper()
				options := pumpOptions{}
				if !crashed {
					options.crashAfter = boundary
				}
				result, err := pump(j, s.image, options)
				if errors.Is(err, errCrash) {
					crashed = true
					result, err = pump(j, s.image, pumpOptions{})
				}
				require.NoError(t, err)
				require.Equal(t, want, result.Kind)
			}
			step("wait")
			dispatch(t, j, 3, 1, 2)
			settle(t, j, 2, "b")
			step("wait")
			dispatch(t, j, 3, 3)
			settle(t, j, 1, "a")
			settle(t, j, 3, "c")
			step("return")
			require.True(t, crashed)
			count, err := j.count()
			require.NoError(t, err)
			require.Equal(t, 3, count)
			history, err := j.trace()
			require.NoError(t, err)
			require.Len(t, history, 7)
			require.JSONEq(t, `["a","c"]`, string(history[6].Value))
		})
	}
}

func (s study) rejectedAdmission(t *testing.T) {
	j := s.run(t, `try {return await agent("original",`+researcher+`);} catch(e) {return {reason:e.code, fallback:await agent("fallback",`+researcher+`)};}`, nil)
	j.denied["researcher"] = true
	_, err := pump(j, s.image, pumpOptions{crashAfter: 2})
	require.ErrorIs(t, err, errCrash, "crash after durable rejection, before JS sees it")
	delete(j.denied, "researcher")
	s.step(t, j, "wait")
	dispatch(t, j, 2, 2)
	settle(t, j, 2, "ok")
	result := s.step(t, j, "return")
	require.JSONEq(t, `{"reason":"admission_denied","fallback":"ok"}`, string(result.Value))
	var accepted int
	require.NoError(t, j.pool.QueryRow(j.ctx, `SELECT count(*) FROM study_invocations WHERE run_id=$1 AND accepted`, j.id).Scan(&accepted))
	require.Equal(t, 1, accepted, "old denied call must not be admitted under new policy")
}

func (s study) partialFailure(t *testing.T) {
	j := s.run(t, `const a=agent("A",`+researcher+`), b=agent("B",`+researcher+`);
let reason; try {await Promise.all([a,b]);}catch(e){reason=e.code;}
const recovered=await agent("recovery",`+researcher+`); await b; return {reason,recovered};`, nil)
	s.step(t, j, "wait")
	dispatch(t, j, 2, 1, 2)
	require.NoError(t, j.complete(frame{Kind: "deliver", ID: 1, Error: &failure{Code: "child_failed"}}))
	s.step(t, j, "wait")
	dispatch(t, j, 2, 3)
	settle(t, j, 3, "recovered")
	s.step(t, j, "wait") // Catching Promise.all did not finish/cancel sibling B.
	settle(t, j, 2, "sibling")
	result := s.step(t, j, "return")
	require.JSONEq(t, `{"reason":"child_failed","recovered":"recovered"}`, string(result.Value))
}

func (s study) deferredRejection(t *testing.T) {
	for name, expression := range map[string]string{
		"direct":        `agent("B",` + researcher + `)`,
		"derived_then":  `agent("B",` + researcher + `).then(x=>x)`,
		"async_wrapper": `(async()=>await agent("B",` + researcher + `))()`,
	} {
		t.Run(name, func(t *testing.T) {
			j := s.run(t, `const a=agent("A",`+researcher+`), b=`+expression+`;await a;try{return await b;}catch(e){return {reason:e.code};}`, nil)
			s.step(t, j, "wait")
			dispatch(t, j, 2, 1, 2)
			require.NoError(t, j.complete(frame{Kind: "deliver", ID: 2, Error: &failure{Code: "child_failed"}}))
			s.step(t, j, "wait")
			// B's rejection precedes its await. The process must survive even after
			// returning quiescence, so deliver A in the SAME live evaluator too.
			history, err := j.trace()
			require.NoError(t, err)
			var source string
			require.NoError(t, j.pool.QueryRow(j.ctx, `SELECT source FROM study_runs WHERE id=$1`, j.id).Scan(&source))
			e, err := startEvaluator(j.ctx, s.image, source, canonical(nil))
			require.NoError(t, err)
			defer func() { _, cleanupErr := e.close(); require.NoError(t, cleanupErr, "remove live rejection fixture") }()
			for _, expected := range history {
				if expected.Kind == "deliver" {
					wait, readErr := e.read()
					require.NoError(t, readErr)
					require.Equal(t, "wait", wait.Kind)
					require.NoError(t, e.send(expected))
				} else {
					actual, readErr := e.read()
					require.NoError(t, readErr)
					require.Equal(t, canonical(expected), canonical(actual))
				}
			}
			wait, err := e.read()
			require.NoError(t, err)
			require.Equal(t, "wait", wait.Kind)
			time.Sleep(25 * time.Millisecond) // Allow Node's unhandled-rejection turn.
			require.NoError(t, e.send(frame{Kind: "deliver", ID: 1, Value: canonical("a")}))
			terminal, err := e.read()
			require.NoError(t, err)
			require.Equal(t, "return", terminal.Kind)
			require.JSONEq(t, `{"reason":"child_failed"}`, string(terminal.Value))
			s.step(t, j, "wait")
			settle(t, j, 1, "a")
			require.JSONEq(t, `{"reason":"child_failed"}`, string(s.step(t, j, "return").Value))
		})
	}
}

func (s study) loop(t *testing.T) {
	j := s.run(t, `let result; for(let i=0;i<args.rounds;i++){result=await agent("round:"+i,`+researcher+`); if(result.pass)return {round:i,result};} return {exhausted:true};`, map[string]any{"rounds": 3})
	for id := 1; id <= 2; id++ {
		s.step(t, j, "wait")
		dispatch(t, j, 1, id)
		settle(t, j, id, map[string]any{"pass": id == 2})
	}
	result := s.step(t, j, "return")
	require.JSONEq(t, `{"round":1,"result":{"pass":true}}`, string(result.Value))
	count, err := j.count()
	require.NoError(t, err)
	require.Equal(t, 2, count)
}

func (s study) capacity(t *testing.T) {
	j := s.run(t, `return await Promise.all([1,2,3].map(x=>agent("item:"+x,`+researcher+`)));`, nil)
	s.step(t, j, "wait")
	for id := 1; id <= 3; id++ {
		dispatch(t, j, 1, id)
		settle(t, j, id, id)
		if id < 3 {
			s.step(t, j, "wait")
		}
	}
	result := s.step(t, j, "return")
	require.JSONEq(t, `[1,2,3]`, string(result.Value))
}

func (s study) waitHandoff(t *testing.T) {
	j := s.run(t, `return await agent("one",`+researcher+`);`, nil)
	s.step(t, j, "wait")
	dispatch(t, j, 1, 1)
	_, err := pump(j, s.image, pumpOptions{beforeWait: func() { settle(t, j, 1, "arrived_during_wait") }})
	require.NoError(t, err)
	var wake bool
	require.NoError(t, j.pool.QueryRow(j.ctx, `SELECT wake FROM study_runs WHERE id=$1`, j.id).Scan(&wake))
	require.True(t, wake, "handoff cannot erase a concurrent receipt's wakeup")
	s.step(t, j, "return")
}

func (s study) fencing(t *testing.T) {
	j := s.run(t, `return await agent("one",`+researcher+`);`, nil)
	s.step(t, j, "wait")
	dispatch(t, j, 1, 1)
	old, err := j.claim()
	require.NoError(t, err)
	_, err = j.claim()
	require.NoError(t, err)
	err = j.append(old, 1, frame{Kind: "invoke", ID: 2, Args: canonical(map[string]any{"prompt": "stale", "options": map[string]any{"agent": "researcher"}})})
	require.ErrorIs(t, err, errFenced)
	_, err = j.pool.Exec(j.ctx, `UPDATE study_runs SET state='paused' WHERE id=$1`, j.id)
	require.NoError(t, err)
	settle(t, j, 1, "settles_while_paused")
	_, err = pump(j, s.image, pumpOptions{})
	require.ErrorIs(t, err, errFenced)
	_, err = j.pool.Exec(j.ctx, `UPDATE study_runs SET state='stopped' WHERE id=$1`, j.id)
	require.NoError(t, err)
	require.ErrorIs(t, j.complete(frame{Kind: "deliver", ID: 1, Value: canonical("late")}), errFenced)
	count, err := j.count()
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func (s study) traceMismatch(t *testing.T) {
	j := s.run(t, `return await agent("original",`+researcher+`);`, nil)
	s.step(t, j, "wait")
	_, err := pump(j, s.image, pumpOptions{sourceFault: `return await agent("changed",` + researcher + `);`})
	require.Error(t, err)
	require.Contains(t, err.Error(), "replay_mismatch")
	count, err := j.count()
	require.NoError(t, err)
	require.Equal(t, 1, count, "mismatching prefix cannot append a new effect")
}

func (s study) profile(t *testing.T) {
	t.Run("ambient_capabilities", func(t *testing.T) {
		j := s.run(t, `return [typeof process,typeof require,typeof fetch,typeof std,typeof os,typeof Date,typeof Intl,typeof Math.random,typeof eval,typeof Function,typeof (async function(){}).constructor,typeof Promise.race,typeof Promise.any,typeof __study_driver];`, nil)
		result := s.step(t, j, "return")
		require.JSONEq(t, `[
"undefined","undefined","undefined","undefined","undefined","undefined","undefined",
"undefined","undefined","undefined","undefined","undefined","undefined","undefined"]`, string(result.Value))
	})
	for name, source := range map[string]string{
		"dynamic_import":       `try{await import("node:fs");return "escaped";}catch(_){return "blocked";}`,
		"constructor_escape":   `try{({}).constructor.constructor("return process")();return "escaped";}catch(_){return "blocked";}`,
		"ordinary_range_error": `try{new Array(-1);return "escaped";}catch(_){return "blocked";}`,
		"intrinsic_binding":    `try{globalThis.Promise={};return "escaped";}catch(_){return "blocked";}`,
	} {
		t.Run(name, func(t *testing.T) {
			j := s.run(t, source, nil)
			require.JSONEq(t, `"blocked"`, string(s.step(t, j, "return").Value))
		})
	}
	for name, source := range map[string]string{
		"orphan":           `await new Promise(()=>{}); return "unreachable";`,
		"outstanding":      `agent("unjoined",` + researcher + `); return "early";`,
		"oversized_return": `return "x".repeat(40000);`,
		"non_json_return":  `return {callback:()=>1};`,
	} {
		t.Run(name, func(t *testing.T) {
			j := s.run(t, source, nil)
			s.step(t, j, "failure")
		})
	}
}

func (s study) resources(t *testing.T) {
	for name, source := range map[string]string{
		"infinite_loop":    `while(true){}`,
		"catch_timeout":    `try{while(true){}}catch(_){await agent("escaped",` + researcher + `);}return "escaped";`,
		"microtasks":       `async function spin(){await 1;return spin();}return await spin();`,
		"allocation":       `const values=[];while(true){values.push(new Array(1000000).fill(1));}`,
		"catch_allocation": `let values=[];try{while(true){values.push(new Array(1000000).fill(1));}}catch(_){values=null;await agent("escaped",` + researcher + `);}return "escaped";`,
		"catch_stack":      `function recurse(){return recurse();}try{recurse();}catch(_){await agent("escaped",` + researcher + `);}return "escaped";`,
	} {
		t.Run(name, func(t *testing.T) {
			j := s.run(t, source, nil)
			start := time.Now()
			var diagnostics engineExit
			result, err := pump(j, s.image, pumpOptions{diagnostics: &diagnostics})
			require.Less(t, time.Since(start), 10*time.Second)
			require.True(t, diagnostics.Observed, "must observe owned container termination")
			require.Contains(t, []int{65, 133, 134, 137, 139}, diagnostics.Code)
			t.Logf("resource=%s exit=%d oom_killed=%t", name, diagnostics.Code, diagnostics.OOMKilled)
			if err == nil {
				require.Equal(t, "failure", result.Kind)
			}
			count, countErr := j.count()
			require.NoError(t, countErr)
			require.Zero(t, count, "resource termination cannot be caught to start work")
		})
	}
	for name, source := range map[string]string{
		"oom_after_receipt":   `let values=[];try{while(true){values.push(new Array(1000000).fill(1));}}catch(_){values=null;await agent("escaped",` + researcher + `);}return "escaped";`,
		"stack_after_receipt": `function recurse(){return recurse();}try{recurse();}catch(_){await agent("escaped",` + researcher + `);}return "escaped";`,
	} {
		t.Run(name, func(t *testing.T) {
			j := s.run(t, `const previous=await agent("committed",`+researcher+`);`+source, nil)
			s.step(t, j, "wait")
			dispatch(t, j, 1, 1)
			settle(t, j, 1, "retained")
			var diagnostics engineExit
			result, err := pump(j, s.image, pumpOptions{diagnostics: &diagnostics})
			if err == nil {
				require.Equal(t, "failure", result.Kind)
			}
			require.True(t, diagnostics.Observed)
			require.Contains(t, []int{65, 133, 134, 137, 139}, diagnostics.Code)
			count, countErr := j.count()
			require.NoError(t, countErr)
			require.Equal(t, 1, count)
			var receipt []byte
			require.NoError(t, j.pool.QueryRow(j.ctx, `SELECT outcome FROM study_receipts WHERE run_id=$1 AND id=1`, j.id).Scan(&receipt))
			require.Contains(t, string(receipt), "retained")
		})
	}
}

func (s study) measure(t *testing.T) {
	j := s.run(t, `const results=await Promise.all(Array.from({length:64},(_,i)=>agent("item:"+i,`+researcher+`)));const tail=await agent("tail",`+researcher+`);return {count:results.length,tail};`, nil)
	start := time.Now()
	s.step(t, j, "wait")
	cold := time.Since(start)
	ids, err := j.dispatch(64)
	require.NoError(t, err)
	require.Len(t, ids, 64)
	for batch := 0; batch < 4; batch++ {
		for id := 64 - batch*16; id > 48-batch*16; id-- {
			settle(t, j, id, string(bytes.Repeat([]byte("x"), 512)))
		}
		s.step(t, j, "wait")
	}
	start = time.Now()
	s.step(t, j, "wait")
	replay := time.Since(start)
	history, err := j.trace()
	require.NoError(t, err)
	require.Len(t, history, 129)
	t.Logf("fanout=64 inline_result_bytes=512 entries=%d trace_bytes=%d cold=%s cached_replay=%s", len(history), len(canonical(history)), cold, replay)
	dispatch(t, j, 64, 65)
	settle(t, j, 65, "done")
	require.JSONEq(t, `{"count":64,"tail":"done"}`, string(s.step(t, j, "return").Value))
}
