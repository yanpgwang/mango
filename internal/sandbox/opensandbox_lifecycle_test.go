package sandbox

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

type reconciliationOpenSandboxService struct {
	openSandboxService
	resources []openSandboxResource
	deleted   []string
	deleteErr map[string]error
}

func (s *reconciliationOpenSandboxService) List(
	context.Context,
	map[string]string,
) ([]openSandboxResource, error) {
	return append([]openSandboxResource(nil), s.resources...), nil
}

func (s *reconciliationOpenSandboxService) Get(
	_ context.Context,
	id string,
) (openSandboxResource, error) {
	for _, resource := range s.resources {
		if resource.id == id {
			return resource, nil
		}
	}
	return openSandboxResource{}, &opensandbox.APIError{StatusCode: 404}
}

func (s *reconciliationOpenSandboxService) Delete(_ context.Context, id string) error {
	s.deleted = append(s.deleted, id)
	return s.deleteErr[id]
}

func TestOpenSandboxReconciliationConvergesConcurrentCreates(t *testing.T) {
	service := &reconciliationOpenSandboxService{}
	provider := &openSandboxProvider{service: service}
	resources := []openSandboxResource{
		{id: "sbx_c", metadata: remoteMetadata("sess_1")},
		{id: "sbx_a", metadata: remoteMetadata("sess_1")},
		{id: "sbx_b", metadata: remoteMetadata("sess_1")},
	}
	selected, err := provider.reconcileResources(
		context.Background(), "sess_1", "", resources,
	)
	if err != nil {
		t.Fatal(err)
	}
	if selected.id != "sbx_a" {
		t.Fatalf("selected sandbox = %q, want sbx_a", selected.id)
	}
	if want := []string{"sbx_b", "sbx_c"}; !reflect.DeepEqual(service.deleted, want) {
		t.Fatalf("deleted sandboxes = %v, want %v", service.deleted, want)
	}
}

func TestOpenSandboxReconciliationPreservesDurableReference(t *testing.T) {
	service := &reconciliationOpenSandboxService{}
	provider := &openSandboxProvider{service: service}
	selected, err := provider.reconcileResources(
		context.Background(),
		"sess_1",
		"sbx_b",
		[]openSandboxResource{
			{id: "sbx_a", metadata: remoteMetadata("sess_1")},
			{id: "sbx_b", metadata: remoteMetadata("sess_1")},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if selected.id != "sbx_b" || !reflect.DeepEqual(service.deleted, []string{"sbx_a"}) {
		t.Fatalf("selected=%q deleted=%v", selected.id, service.deleted)
	}
}

func TestOpenSandboxReconciliationFailsWithoutDeletingForeignResource(t *testing.T) {
	service := &reconciliationOpenSandboxService{}
	provider := &openSandboxProvider{service: service}
	_, err := provider.reconcileResources(
		context.Background(),
		"sess_1",
		"",
		[]openSandboxResource{
			{id: "sbx_a", metadata: remoteMetadata("sess_1")},
			{id: "sbx_foreign", metadata: remoteMetadata("sess_2")},
		},
	)
	if err == nil || !IsPermanent(err) {
		t.Fatalf("ownership error = %v, want permanent", err)
	}
	if len(service.deleted) != 0 {
		t.Fatalf("deleted foreign resources: %v", service.deleted)
	}
}

func TestOpenSandboxReconciliationRetriesCleanupFailure(t *testing.T) {
	cleanupErr := errors.New("control plane unavailable")
	service := &reconciliationOpenSandboxService{
		deleteErr: map[string]error{"sbx_b": cleanupErr},
	}
	provider := &openSandboxProvider{service: service}
	_, err := provider.reconcileResources(
		context.Background(),
		"sess_1",
		"",
		[]openSandboxResource{
			{id: "sbx_a", metadata: remoteMetadata("sess_1")},
			{id: "sbx_b", metadata: remoteMetadata("sess_1")},
		},
	)
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("cleanup error = %v, want %v", err, cleanupErr)
	}
}

func TestOpenSandboxDestroyBoundSessionRemovesDuplicatesBeforeDurableReference(t *testing.T) {
	service := &reconciliationOpenSandboxService{
		resources: []openSandboxResource{
			{id: "sbx_a", metadata: remoteMetadata("sess_1")},
			{id: "sbx_b", metadata: remoteMetadata("sess_1")},
		},
	}
	provider := &openSandboxProvider{service: service}
	err := provider.DestroyBoundSession(
		context.Background(), "sess_1", Ref{Provider: provider.Name(), ID: "sbx_a"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"sbx_b", "sbx_a"}; !reflect.DeepEqual(service.deleted, want) {
		t.Fatalf("destroy order = %v, want %v", service.deleted, want)
	}
}

func TestOpenSandboxDestroyBoundSessionValidatesAllOwnershipBeforeDelete(t *testing.T) {
	service := &reconciliationOpenSandboxService{
		resources: []openSandboxResource{
			{id: "sbx_a", metadata: remoteMetadata("sess_1")},
			{id: "sbx_foreign", metadata: remoteMetadata("sess_2")},
		},
	}
	provider := &openSandboxProvider{service: service}
	err := provider.DestroyBoundSession(
		context.Background(), "sess_1", Ref{Provider: provider.Name(), ID: "sbx_a"},
	)
	if err == nil || !IsPermanent(err) {
		t.Fatalf("ownership error = %v, want permanent", err)
	}
	if len(service.deleted) != 0 {
		t.Fatalf("deleted resources before ownership validation: %v", service.deleted)
	}
}

type electionOpenSandboxService struct {
	resources map[string]openSandboxResource
	deleted   []string
	listCalls int
}

func (s *electionOpenSandboxService) List(
	context.Context,
	map[string]string,
) ([]openSandboxResource, error) {
	s.listCalls++
	if s.listCalls == 1 {
		return nil, nil
	}
	if s.listCalls == 2 {
		resource, ok := s.resources["sbx_loser"]
		if !ok {
			return nil, nil
		}
		return []openSandboxResource{resource}, nil
	}
	resources := make([]openSandboxResource, 0, len(s.resources))
	for _, resource := range s.resources {
		resources = append(resources, resource)
	}
	return resources, nil
}

func (s *electionOpenSandboxService) Get(
	_ context.Context,
	id string,
) (openSandboxResource, error) {
	resource, ok := s.resources[id]
	if !ok {
		return openSandboxResource{}, &opensandbox.APIError{StatusCode: 404}
	}
	return resource, nil
}

func (s *electionOpenSandboxService) Create(
	_ context.Context,
	sessionKey string,
	_ Spec,
) (openSandboxRemote, error) {
	resource := openSandboxResource{
		id: "sbx_loser", metadata: remoteMetadata(sessionKey),
	}
	s.resources[resource.id] = resource
	return &electionOpenSandboxRemote{service: s, id: resource.id}, nil
}

func (s *electionOpenSandboxService) Connect(
	_ context.Context,
	id string,
) (openSandboxRemote, error) {
	if _, ok := s.resources[id]; !ok {
		return nil, &opensandbox.APIError{StatusCode: 404}
	}
	return &electionOpenSandboxRemote{service: s, id: id}, nil
}

func (s *electionOpenSandboxService) Delete(_ context.Context, id string) error {
	if _, ok := s.resources[id]; !ok {
		return &opensandbox.APIError{StatusCode: 404}
	}
	delete(s.resources, id)
	s.deleted = append(s.deleted, id)
	return nil
}

type electionOpenSandboxRemote struct {
	openSandboxRemote
	service *electionOpenSandboxService
	id      string
}

func (r *electionOpenSandboxRemote) ID() string { return r.id }

func (*electionOpenSandboxRemote) Exec(
	context.Context,
	string,
	string,
	time.Duration,
	*int32,
	*int32,
) (string, string, int, error) {
	return "", "", 0, nil
}

func (*electionOpenSandboxRemote) ResourceStat(
	context.Context,
	string,
) (remoteFileInfo, error) {
	return remoteFileInfo{Directory: true}, nil
}

func (r *electionOpenSandboxRemote) Destroy(ctx context.Context) error {
	return r.service.Delete(ctx, r.id)
}

func TestOpenSandboxLosingBindingElectionDeletesOnlyLoser(t *testing.T) {
	ctx := context.Background()
	service := &electionOpenSandboxService{resources: map[string]openSandboxResource{
		"sbx_winner": {id: "sbx_winner", metadata: remoteMetadata("sess_1")},
	}}
	provider := &openSandboxProvider{service: service, root: remoteDefaultRoot}
	winner := Ref{Provider: provider.Name(), ID: "sbx_winner"}
	bindings := &authoritativeBindingStore{winner: Binding{
		SessionID: "sess_1", Ref: winner, SpecHash: specHash(Spec{}),
	}}
	manager := NewSessionManager(provider, bindings)
	box, err := manager.Acquire(ctx, "sess_1", Spec{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = box.Destroy(context.Background()) })
	if _, ok := service.resources[winner.ID]; !ok {
		t.Fatal("losing binding cleanup deleted the durable winner")
	}
	if _, ok := service.resources["sbx_loser"]; ok {
		t.Fatal("losing sandbox survived binding election")
	}
	if want := []string{"sbx_loser"}; !reflect.DeepEqual(service.deleted, want) {
		t.Fatalf("deleted sandboxes = %v, want %v", service.deleted, want)
	}
}
