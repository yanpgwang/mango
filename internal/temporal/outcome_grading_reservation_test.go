package temporal

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
)

type gradingReservationSource struct {
	*fakeSource
	err          error
	reservations []domain.OutcomeGradingReservation
}

func (s *gradingReservationSource) ReserveOutcomeGrading(_ context.Context, in domain.OutcomeGradingReservation) error {
	s.reservations = append(s.reservations, in)
	return s.err
}

func TestEvaluateOutcomeReservesBeforeProviderIOAndRetriesSameIdentity(t *testing.T) {
	source := &gradingReservationSource{fakeSource: newFakeSource(nil), err: errors.New("control wait: open Run")}
	client := &outcomeModel{response: model.Response{Content: []domain.ContentBlock{{Type: "text", Text: `{"result":"satisfied","explanation":"ok"}`}}}}
	activities := NewActivities(client, source, nil, domain.NewSeqIDGen())
	input := EvaluateOutcomeInput{SessionID: "sesn_grading", StartEventID: "sevt_start", EndEventID: "sevt_end", Model: "test-model",
		Outcome:   domain.OutcomeSpec{OutcomeID: "outc_1", Description: "report", Rubric: map[string]any{"type": "text", "content": "evidence"}},
		Candidate: []domain.Message{{Role: domain.RoleAssistant, Content: []domain.ContentBlock{{Type: "text", Text: "candidate"}}}}}
	got, err := activities.EvaluateOutcome(t.Context(), input)
	require.ErrorContains(t, err, "control wait")
	require.Empty(t, got.FatalError, "an admission wait is not a failed business verdict")
	require.Empty(t, client.request.Messages, "the actual provider adapter must not be called before admission")
	require.Len(t, source.reservations, 1)
	source.err = nil
	got, err = activities.EvaluateOutcome(t.Context(), input)
	require.NoError(t, err)
	require.Equal(t, "satisfied", got.Result)
	require.Len(t, source.reservations, 2)
	require.Equal(t, source.reservations[0], source.reservations[1])
	require.Equal(t, domain.OutcomeGradingReservation{SessionID: input.SessionID, OutcomeID: input.Outcome.OutcomeID, StartEventID: input.StartEventID, EndEventID: input.EndEventID}, source.reservations[0])
	// The production adapter must always implement this boundary; optional
	// interfaces only retain non-persistent package doubles for unrelated tests.
	var _ OutcomeGradingSource = storeSource{}
}
