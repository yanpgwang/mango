package httpapi

import (
	"context"
	_ "embed"
	"net/http"
	"time"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/workspace"
)

//go:embed openapi.yaml
var openapiDoc string

const timeFmt = time.RFC3339Nano

type AgentService interface {
	Create(context.Context, domain.Agent) (domain.Agent, error)
	Get(context.Context, string) (domain.Agent, error)
	List(context.Context, app.AgentListQuery) (app.AgentListPage, error)
	Versions(context.Context, string, app.AgentVersionListQuery) (app.AgentVersionListPage, error)
	Update(context.Context, string, domain.AgentPatch) (domain.Agent, error)
	Archive(context.Context, string) (domain.Agent, error)
}

type EnvironmentService interface {
	Create(context.Context, domain.Environment) (domain.Environment, error)
	Update(context.Context, string, domain.EnvironmentPatch) (domain.Environment, error)
	Get(context.Context, string) (domain.Environment, error)
	List(context.Context, app.EnvironmentListQuery) (app.EnvironmentListPage, error)
	Archive(context.Context, string) (domain.Environment, error)
	Delete(context.Context, string) error
}

type EnvironmentWorkService interface {
	Get(context.Context, string, string) (domain.EnvironmentWork, error)
	Update(context.Context, string, string, map[string]*string) (domain.EnvironmentWork, error)
	List(context.Context, string, app.EnvironmentWorkListQuery) (app.EnvironmentWorkListPage, error)
	Ack(context.Context, string, string) (domain.EnvironmentWork, error)
	Heartbeat(context.Context, string, string, *string, *int64) (domain.EnvironmentWorkHeartbeat, error)
	Poll(context.Context, string, string, time.Duration, *time.Duration) (*domain.EnvironmentWork, error)
	Stop(context.Context, string, string, bool) error
	Stats(context.Context, string) (domain.EnvironmentWorkQueueStats, error)
}

type SessionService interface {
	Create(context.Context, app.CreateSessionInput) (domain.Session, error)
	Get(context.Context, string) (domain.Session, error)
	List(context.Context, app.ListPage) (app.SessionListPage, error)
	SendEvent(context.Context, string, []domain.EventDraft) ([]domain.Event, error)
	Update(context.Context, string, domain.SessionUpdate) (domain.Session, error)
	Archive(context.Context, string) (domain.Session, error)
	Delete(context.Context, string) error
}

type EventService interface {
	Query(context.Context, string, app.EventQuery) ([]domain.Event, error)
}

type EventSubscriber interface {
	SubscribeContext(context.Context, string, map[string]bool) (<-chan app.Frame, func(), error)
}

// SessionScopeValidator rechecks a long-lived Session credential after the
// initial HTTP authentication. Implementations must fail closed when the Work
// lease stops, expires, or changes owner.
type SessionScopeValidator interface {
	ValidateSessionScope(context.Context, workspace.SessionScope) error
}

type ThreadEventSubscriber interface {
	SubscribeThreadContext(
		context.Context, string, string, map[string]bool,
	) (<-chan app.Frame, func(), error)
}

type SessionThreadService interface {
	Get(context.Context, string, string) (domain.SessionThread, error)
	List(context.Context, string, app.SessionThreadListQuery) ([]domain.SessionThread, error)
	Archive(context.Context, string, string) (domain.SessionThread, error)
}

type FileService interface {
	Upload(context.Context, app.FileUploadInput) (domain.File, error)
	Get(context.Context, string) (domain.File, error)
	List(context.Context, app.FileListQuery) (app.FileListPage, error)
	Download(context.Context, string) (app.FileDownload, error)
	Delete(context.Context, string) (domain.File, error)
}

type SkillService interface {
	Create(context.Context, app.SkillCreateInput) (domain.Skill, error)
	Get(context.Context, string) (domain.Skill, error)
	List(context.Context, app.SkillListQuery) (app.SkillListPage, error)
	Delete(context.Context, string) (domain.Skill, error)
	CreateVersion(context.Context, string, []app.SkillUploadFile) (domain.SkillVersion, error)
	GetVersion(context.Context, string, string) (domain.SkillVersion, error)
	ListVersions(context.Context, string, app.SkillVersionListQuery) (app.SkillVersionListPage, error)
	DeleteVersion(context.Context, string, string) (domain.SkillVersion, error)
	Download(context.Context, string, string) (app.SkillVersionDownload, error)
}

type MemoryService interface {
	CreateStore(context.Context, app.MemoryStoreCreateInput) (domain.MemoryStore, error)
	GetStore(context.Context, string) (domain.MemoryStore, error)
	UpdateStore(context.Context, string, app.MemoryStoreUpdateInput) (domain.MemoryStore, error)
	ListStores(context.Context, app.MemoryStoreListQuery) (app.MemoryStoreListPage, error)
	ArchiveStore(context.Context, string) (domain.MemoryStore, error)
	DeleteStore(context.Context, string) error
	CreateMemory(context.Context, string, app.MemoryCreateInput) (domain.Memory, error)
	GetMemory(context.Context, string, string) (domain.Memory, error)
	ListMemories(context.Context, string, app.MemoryListQuery) (app.MemoryListPage, error)
	UpdateMemory(context.Context, string, string, app.MemoryUpdateInput) (domain.Memory, error)
	DeleteMemory(context.Context, string, string, *string, domain.MemoryActor) (domain.Memory, error)
	GetMemoryVersion(context.Context, string, string) (domain.MemoryVersion, error)
	ListMemoryVersions(context.Context, string, app.MemoryVersionListQuery) (app.MemoryVersionListPage, error)
	RedactMemoryVersion(context.Context, string, string, domain.MemoryActor) (domain.MemoryVersion, error)
}

type VaultService interface {
	CreateVault(context.Context, app.VaultCreateInput) (domain.Vault, error)
	GetVault(context.Context, string) (domain.Vault, error)
	UpdateVault(context.Context, string, app.VaultUpdateInput) (domain.Vault, error)
	ListVaults(context.Context, app.VaultListQuery) (app.VaultListPage, error)
	ArchiveVault(context.Context, string) (domain.Vault, error)
	DeleteVault(context.Context, string) error
	CreateCredential(context.Context, string, app.CredentialCreateInput) (domain.VaultCredential, error)
	GetCredential(context.Context, string, string) (domain.VaultCredential, error)
	UpdateCredential(context.Context, string, string, app.CredentialUpdateInput) (domain.VaultCredential, error)
	ListCredentials(context.Context, string, app.CredentialListQuery) (app.CredentialListPage, error)
	ArchiveCredential(context.Context, string, string) (domain.VaultCredential, error)
	DeleteCredential(context.Context, string, string) error
	ValidateMCPOAuthCredential(context.Context, string, string) (app.CredentialValidation, error)
}

type WebhookService interface {
	CreateWebhook(context.Context, app.WebhookCreateInput) (app.WebhookSecretResult, error)
	GetWebhook(context.Context, string) (domain.Webhook, error)
	UpdateWebhook(context.Context, string, app.WebhookUpdateInput) (domain.Webhook, error)
	ListWebhooks(context.Context, app.WebhookListQuery) (app.WebhookListPage, error)
	RegenerateSigningSecret(context.Context, string) (app.WebhookSecretResult, error)
	DeleteWebhook(context.Context, string) error
}

type DeploymentService interface {
	Create(context.Context, app.DeploymentCreateInput) (domain.Deployment, error)
	Get(context.Context, string) (domain.Deployment, error)
	Update(context.Context, string, domain.DeploymentPatch) (domain.Deployment, error)
	List(context.Context, app.DeploymentListQuery) (app.DeploymentListPage, error)
	Archive(context.Context, string) (domain.Deployment, error)
	Pause(context.Context, string) (domain.Deployment, error)
	Unpause(context.Context, string) (domain.Deployment, error)
	Run(context.Context, string) (domain.DeploymentRun, error)
	GetRun(context.Context, string) (domain.DeploymentRun, error)
	ListRuns(context.Context, app.DeploymentRunListQuery) (app.DeploymentRunListPage, error)
}

type SessionResourceService interface {
	Add(context.Context, string, app.FileSessionResourceInput) (domain.SessionResource, error)
	Get(context.Context, string, string) (domain.SessionResource, error)
	List(context.Context, string, app.SessionResourceListQuery) (app.SessionResourceListPage, error)
	Delete(context.Context, string, string) (domain.SessionResource, error)
}

type Deps struct {
	Agents           AgentService
	Envs             EnvironmentService
	Sessions         SessionService
	Threads          SessionThreadService
	Events           EventService
	Stream           EventSubscriber
	Files            FileService
	Skills           SkillService
	Memory           MemoryService
	Vaults           VaultService
	Webhooks         WebhookService
	Deployments      DeploymentService
	EnvironmentWork  EnvironmentWorkService
	SessionResources SessionResourceService
}

type Server struct {
	deps Deps
	cfg  Config
	mux  *http.ServeMux
}

func NewServer(deps Deps, cfg Config) *Server {
	s := &Server{deps: deps, cfg: cfg, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	s.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	s.mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write([]byte(openapiDoc))
	})

	s.mux.HandleFunc("POST /v1/agents", s.createAgent)
	s.mux.HandleFunc("GET /v1/agents", s.listAgents)
	s.mux.HandleFunc("GET /v1/agents/{id}", s.getAgent)
	s.mux.HandleFunc("POST /v1/agents/{id}", s.updateAgent)
	s.mux.HandleFunc("GET /v1/agents/{id}/versions", s.listAgentVersions)
	s.mux.HandleFunc("POST /v1/agents/{id}/archive", s.archiveAgent)

	s.mux.HandleFunc("POST /v1/environments", s.createEnvironment)
	s.mux.HandleFunc("GET /v1/environments", s.listEnvironments)
	s.mux.HandleFunc("GET /v1/environments/{id}", s.getEnvironment)
	s.mux.HandleFunc("POST /v1/environments/{id}", s.updateEnvironment)
	s.mux.HandleFunc("POST /v1/environments/{id}/archive", s.archiveEnvironment)
	s.mux.HandleFunc("DELETE /v1/environments/{id}", s.deleteEnvironment)
	s.mux.HandleFunc("GET /v1/environments/{environment_id}/work", s.listEnvironmentWork)
	s.mux.HandleFunc("GET /v1/environments/{environment_id}/work/poll", s.pollEnvironmentWork)
	s.mux.HandleFunc("GET /v1/environments/{environment_id}/work/stats", s.environmentWorkStats)
	s.mux.HandleFunc("GET /v1/environments/{environment_id}/work/{work_id}", s.getEnvironmentWork)
	s.mux.HandleFunc("POST /v1/environments/{environment_id}/work/{work_id}", s.updateEnvironmentWork)
	s.mux.HandleFunc("POST /v1/environments/{environment_id}/work/{work_id}/ack", s.ackEnvironmentWork)
	s.mux.HandleFunc("POST /v1/environments/{environment_id}/work/{work_id}/heartbeat", s.heartbeatEnvironmentWork)
	s.mux.HandleFunc("POST /v1/environments/{environment_id}/work/{work_id}/stop", s.stopEnvironmentWork)

	s.mux.HandleFunc("POST /v1/files", s.uploadFile)
	s.mux.HandleFunc("GET /v1/files", s.listFiles)
	s.mux.HandleFunc("GET /v1/files/{id}", s.getFile)
	s.mux.HandleFunc("GET /v1/files/{id}/content", s.downloadFile)
	s.mux.HandleFunc("DELETE /v1/files/{id}", s.deleteFile)

	s.mux.HandleFunc("POST /v1/skills", s.createSkill)
	s.mux.HandleFunc("GET /v1/skills", s.listSkills)
	s.mux.HandleFunc("GET /v1/skills/{id}", s.getSkill)
	s.mux.HandleFunc("DELETE /v1/skills/{id}", s.deleteSkill)
	s.mux.HandleFunc("POST /v1/skills/{id}/versions", s.createSkillVersion)
	s.mux.HandleFunc("GET /v1/skills/{id}/versions", s.listSkillVersions)
	s.mux.HandleFunc("GET /v1/skills/{id}/versions/{version}", s.getSkillVersion)
	s.mux.HandleFunc("DELETE /v1/skills/{id}/versions/{version}", s.deleteSkillVersion)
	s.mux.HandleFunc("GET /v1/skills/{id}/versions/{version}/content", s.downloadSkillVersion)

	s.mux.HandleFunc("POST /v1/memory_stores", s.createMemoryStore)
	s.mux.HandleFunc("GET /v1/memory_stores", s.listMemoryStores)
	s.mux.HandleFunc("GET /v1/memory_stores/{store_id}", s.getMemoryStore)
	s.mux.HandleFunc("POST /v1/memory_stores/{store_id}", s.updateMemoryStore)
	s.mux.HandleFunc("DELETE /v1/memory_stores/{store_id}", s.deleteMemoryStore)
	s.mux.HandleFunc("POST /v1/memory_stores/{store_id}/archive", s.archiveMemoryStore)
	s.mux.HandleFunc("POST /v1/memory_stores/{store_id}/memories", s.createMemory)
	s.mux.HandleFunc("GET /v1/memory_stores/{store_id}/memories", s.listMemories)
	s.mux.HandleFunc("GET /v1/memory_stores/{store_id}/memories/{memory_id}", s.getMemory)
	s.mux.HandleFunc("POST /v1/memory_stores/{store_id}/memories/{memory_id}", s.updateMemory)
	s.mux.HandleFunc("DELETE /v1/memory_stores/{store_id}/memories/{memory_id}", s.deleteMemory)
	s.mux.HandleFunc("GET /v1/memory_stores/{store_id}/memory_versions", s.listMemoryVersions)
	s.mux.HandleFunc("GET /v1/memory_stores/{store_id}/memory_versions/{version_id}", s.getMemoryVersion)
	s.mux.HandleFunc("POST /v1/memory_stores/{store_id}/memory_versions/{version_id}/redact", s.redactMemoryVersion)

	s.mux.HandleFunc("POST /v1/vaults", s.createVault)
	s.mux.HandleFunc("GET /v1/vaults", s.listVaults)
	s.mux.HandleFunc("GET /v1/vaults/{vault_id}", s.getVault)
	s.mux.HandleFunc("POST /v1/vaults/{vault_id}", s.updateVault)
	s.mux.HandleFunc("POST /v1/vaults/{vault_id}/archive", s.archiveVault)
	s.mux.HandleFunc("DELETE /v1/vaults/{vault_id}", s.deleteVault)
	s.mux.HandleFunc("POST /v1/vaults/{vault_id}/credentials", s.createCredential)
	s.mux.HandleFunc("GET /v1/vaults/{vault_id}/credentials", s.listCredentials)
	s.mux.HandleFunc("GET /v1/vaults/{vault_id}/credentials/{credential_id}", s.getCredential)
	s.mux.HandleFunc("POST /v1/vaults/{vault_id}/credentials/{credential_id}", s.updateCredential)
	s.mux.HandleFunc("POST /v1/vaults/{vault_id}/credentials/{credential_id}/archive", s.archiveCredential)
	s.mux.HandleFunc("POST /v1/vaults/{vault_id}/credentials/{credential_id}/mcp_oauth_validate", s.validateMCPOAuthCredential)
	s.mux.HandleFunc("DELETE /v1/vaults/{vault_id}/credentials/{credential_id}", s.deleteCredential)

	s.mux.HandleFunc("POST /v1/webhooks", s.createWebhook)
	s.mux.HandleFunc("GET /v1/webhooks", s.listWebhooks)
	s.mux.HandleFunc("GET /v1/webhooks/{webhook_id}", s.getWebhook)
	s.mux.HandleFunc("POST /v1/webhooks/{webhook_id}", s.updateWebhook)
	s.mux.HandleFunc("DELETE /v1/webhooks/{webhook_id}", s.deleteWebhook)
	s.mux.HandleFunc("POST /v1/webhooks/{webhook_id}/regenerate_signing_secret", s.regenerateWebhookSigningSecret)

	s.mux.HandleFunc("POST /v1/deployments", s.createDeployment)
	s.mux.HandleFunc("GET /v1/deployments", s.listDeployments)
	s.mux.HandleFunc("GET /v1/deployments/{deployment_id}", s.getDeployment)
	s.mux.HandleFunc("POST /v1/deployments/{deployment_id}", s.updateDeployment)
	s.mux.HandleFunc("POST /v1/deployments/{deployment_id}/archive", s.archiveDeployment)
	s.mux.HandleFunc("POST /v1/deployments/{deployment_id}/pause", s.pauseDeployment)
	s.mux.HandleFunc("POST /v1/deployments/{deployment_id}/unpause", s.unpauseDeployment)
	s.mux.HandleFunc("POST /v1/deployments/{deployment_id}/run", s.runDeployment)
	s.mux.HandleFunc("GET /v1/deployment_runs", s.listDeploymentRuns)
	s.mux.HandleFunc("GET /v1/deployment_runs/{deployment_run_id}", s.getDeploymentRun)

	s.registerSessionRoutes()
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, domain.NotFound("resource not found"))
	})
}

func (s *Server) Handler() http.Handler {
	return requestIDMiddleware(bodyLimitMiddleware(authMiddleware(s.cfg,
		contentTypeMiddleware(s.mux))))
}
