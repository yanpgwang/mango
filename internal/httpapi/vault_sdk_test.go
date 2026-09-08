package httpapi

import (
	"bytes"
	"context"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	mango "github.com/yanpgwang/mango/sdk/go"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/credentialruntime"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/secretcrypto"
)

func TestSDK_VaultAndStaticBearerCredentialLifecycle(t *testing.T) {
	repo := newHTTPVaultRepository()
	keyring, err := secretcrypto.NewAESGCMKeyring("test", map[string][]byte{
		"test": bytes.Repeat([]byte{11}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	service := app.NewVaultService(app.VaultServiceConfig{
		Repository: repo, Cipher: keyring, IDGenerator: domain.NewSeqIDGen(),
		Clock: domain.FixedClock{T: time.Unix(1000, 0).UTC()}, MCPValidator: sdkVaultValidator{},
	})
	server := httptest.NewServer(NewServer(Deps{Vaults: service}, Config{
		RequireAuth: true,
	}).Handler())
	t.Cleanup(server.Close)
	client, responseJSON := recordedSDKClient(t, server.URL)
	ctx := context.Background()

	vault, err := client.Vaults.New(ctx, sdkBody[mango.VaultCreateRequest](t, `{"display_name":"Production tools","metadata":{"team":"platform"}}`))
	if err != nil {
		t.Fatalf("create vault: %v", err)
	}
	if vault.Type != "vault" || vault.DisplayName != "Production tools" {
		t.Fatalf("vault = %s", responseJSON())
	}
	gotVault, err := client.Vaults.Get(ctx, vault.ID)
	if err != nil || gotVault.ID != vault.ID {
		t.Fatalf("get vault = %#v, %v", gotVault, err)
	}
	updatedVault, err := client.Vaults.Update(ctx, vault.ID, sdkBody[mango.VaultUpdateRequest](t, `{"display_name":"Production MCP tools"}`))
	if err != nil || updatedVault.DisplayName != "Production MCP tools" {
		t.Fatalf("update vault = %#v, %v", updatedVault, err)
	}
	updatedVault, err = client.Vaults.Update(ctx, vault.ID, sdkBody[mango.VaultUpdateRequest](t, `{"display_name":null,"metadata":null}`))
	if err != nil || updatedVault.DisplayName != "Production MCP tools" || len(updatedVault.Metadata) != 0 {
		t.Fatalf("nullable vault update = %#v, %v", updatedVault, err)
	}
	if _, err := client.Vaults.New(ctx, sdkBody[mango.VaultCreateRequest](t, `{"display_name":"Development tools"}`)); err != nil {
		t.Fatalf("create second vault: %v", err)
	}
	vaultPager := client.Vaults.ListAutoPaging(ctx, mango.ListVaultsParams{Limit: mango.Some[int64](1)})
	vaultCount := 0
	for vaultPager.Next() {
		vaultCount++
	}
	if err := vaultPager.Err(); err != nil || vaultCount != 2 {
		t.Fatalf("vault auto-pagination count = %d, err = %v", vaultCount, err)
	}

	credential, err := client.Vaults.Credentials.New(ctx, vault.ID, sdkBody[mango.VaultCredentialCreateRequest](t, `{"display_name":"Build MCP","auth":{"type":"static_bearer","mcp_server_url":"https://MCP.example:443/api","token":"sdk-secret-token"}}`))
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}
	if credential.Auth.StaticBearerCredentialAuth.MCPServerURL != "https://MCP.example:443/api" {
		t.Fatalf("credential = %s", responseJSON())
	}
	if strings.Contains(responseJSON(), "sdk-secret-token") || strings.Contains(responseJSON(), "cipher") {
		t.Fatalf("credential response leaked secret material: %s", responseJSON())
	}

	got, err := client.Vaults.Credentials.Get(ctx, vault.ID, credential.ID)
	if err != nil || got.ID != credential.ID {
		t.Fatalf("get credential = %#v, %v", got, err)
	}
	updated, err := client.Vaults.Credentials.Update(ctx, vault.ID, credential.ID, sdkBody[mango.VaultCredentialUpdateRequest](t, `{"display_name":null,"metadata":null,"auth":{"type":"static_bearer","token":null}}`))
	if err != nil || !strings.Contains(responseJSON(), `"display_name":null`) || !strings.Contains(responseJSON(), `"metadata":{}`) {
		t.Fatalf("update credential = %#v, %v", updated, err)
	}
	page, err := client.Vaults.Credentials.List(ctx, vault.ID, mango.ListVaultCredentialsParams{})
	if err != nil || len(page.Data) != 1 || page.Data[0].ID != credential.ID || strings.Contains(responseJSON(), `"has_more"`) {
		t.Fatalf("credential page = %#v, %v", page, err)
	}
	archived, err := client.Vaults.Credentials.Archive(ctx, vault.ID, credential.ID)
	if err != nil || archived.ArchivedAt == nil {
		t.Fatalf("archive credential = %#v, %v", archived, err)
	}
	deleted, err := client.Vaults.Credentials.Delete(ctx, vault.ID, credential.ID)
	if err != nil || deleted.Type != "vault_credential_deleted" {
		t.Fatalf("delete credential = %#v, %v", deleted, err)
	}
	oauth, err := client.Vaults.Credentials.New(ctx, vault.ID, sdkBody[mango.VaultCredentialCreateRequest](t, `{"display_name":null,"auth":{"type":"mcp_oauth","mcp_server_url":"https://oauth-mcp.example/mcp","access_token":"oauth-access-secret","expires_at":"1970-01-01T01:06:40Z","refresh":{"client_id":"client-id","refresh_token":"oauth-refresh-secret","token_endpoint":"https://auth.example/token","resource":null,"scope":"openid","token_endpoint_auth":{"type":"client_secret_basic","client_secret":"oauth-client-secret"}}}}`))
	if err != nil {
		t.Fatalf("create OAuth credential: %v", err)
	}
	for _, secret := range []string{"oauth-access-secret", "oauth-refresh-secret", "oauth-client-secret"} {
		if strings.Contains(responseJSON(), secret) {
			t.Fatalf("OAuth response leaked %q: %s", secret, responseJSON())
		}
	}
	publicOAuth := oauth.Auth.MCPOAuthCredentialAuth
	if publicOAuth.Refresh.ClientID != "client-id" || publicOAuth.Refresh.TokenEndpoint != "https://auth.example/token" {
		t.Fatalf("OAuth public auth = %s", responseJSON())
	}
	validation, err := client.Vaults.Credentials.ValidateMCPOAuth(ctx, vault.ID, oauth.ID)
	if err != nil || validation.Status != "valid" ||
		validation.CredentialID != oauth.ID || validation.VaultID != vault.ID ||
		!validation.HasRefreshToken || !strings.Contains(responseJSON(), `"mcp_probe":null`) ||
		!strings.Contains(responseJSON(), `"refresh":null`) {
		t.Fatalf("OAuth validation = %#v, %v", validation, err)
	}
	updatedOAuth, err := client.Vaults.Credentials.Update(ctx, vault.ID, oauth.ID, sdkBody[mango.VaultCredentialUpdateRequest](t, `{"auth":{"type":"mcp_oauth","refresh":{"refresh_token":null,"scope":null,"token_endpoint_auth":{"type":"client_secret_basic","client_secret":null}}}}`))
	if err != nil || !strings.Contains(responseJSON(), `"scope":null`) || strings.Contains(responseJSON(), "oauth-refresh-secret") {
		t.Fatalf("nested nullable OAuth update = %#v, %v", updatedOAuth, err)
	}
	updatedOAuth, err = client.Vaults.Credentials.Update(ctx, vault.ID, oauth.ID, sdkBody[mango.VaultCredentialUpdateRequest](t, `{"metadata":null,"auth":{"type":"mcp_oauth","access_token":null,"expires_at":null,"refresh":null}}`))
	if err != nil || !strings.Contains(responseJSON(), `"expires_at":null`) || !strings.Contains(responseJSON(), `"refresh":null`) {
		t.Fatalf("nullable OAuth update = %#v, %v", updatedOAuth, err)
	}
	pagerCredential, err := client.Vaults.Credentials.New(ctx, vault.ID, sdkBody[mango.VaultCredentialCreateRequest](t, `{"auth":{"type":"static_bearer","mcp_server_url":"https://pager.example/mcp","token":"pager-secret"}}`))
	if err != nil {
		t.Fatalf("create pager credential: %v", err)
	}
	credentialPager := client.Vaults.Credentials.ListAutoPaging(ctx, vault.ID, mango.ListVaultCredentialsParams{Limit: mango.Some[int64](1)})
	credentialIDs := map[string]bool{}
	for credentialPager.Next() {
		credentialIDs[credentialPager.Value().ID] = true
	}
	if err := credentialPager.Err(); err != nil || !credentialIDs[oauth.ID] || !credentialIDs[pagerCredential.ID] || len(credentialIDs) != 2 {
		t.Fatalf("credential auto-pagination IDs = %#v, err = %v", credentialIDs, err)
	}
	nullableCreate, err := client.Vaults.Credentials.New(ctx, vault.ID, sdkBody[mango.VaultCredentialCreateRequest](t, `{"display_name":null,"auth":{"type":"mcp_oauth","mcp_server_url":"https://nullable.example/mcp","access_token":"nullable-access-secret","expires_at":null,"refresh":null}}`))
	if err != nil || !strings.Contains(responseJSON(), `"display_name":null`) || !strings.Contains(responseJSON(), `"expires_at":null`) || !strings.Contains(responseJSON(), `"refresh":null`) {
		t.Fatalf("nullable OAuth create = %#v, %v", nullableCreate, err)
	}
	archivedVault, err := client.Vaults.Archive(ctx, vault.ID)
	if err != nil || archivedVault.ArchivedAt == nil {
		t.Fatalf("archive vault = %#v, %v", archivedVault, err)
	}
	deletedVault, err := client.Vaults.Delete(ctx, vault.ID)
	if err != nil || deletedVault.Type != "vault_deleted" {
		t.Fatalf("delete vault = %#v, %v", deletedVault, err)
	}
}

type sdkVaultValidator struct{}

func (sdkVaultValidator) ValidateBearer(
	context.Context,
	string,
	string,
) (credentialruntime.MCPProbeResult, error) {
	return credentialruntime.MCPProbeResult{Verdict: credentialruntime.VerdictValid}, nil
}

type httpVaultRepository struct {
	mu          sync.Mutex
	vaults      map[string]domain.Vault
	credentials map[string]domain.VaultCredential
}

func newHTTPVaultRepository() *httpVaultRepository {
	return &httpVaultRepository{vaults: map[string]domain.Vault{}, credentials: map[string]domain.VaultCredential{}}
}

func (r *httpVaultRepository) CreateVault(_ context.Context, item domain.Vault) (domain.Vault, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.vaults[item.ID] = item
	return item, nil
}
func (r *httpVaultRepository) GetVault(_ context.Context, id string) (domain.Vault, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.vaults[id]
	if !ok {
		return domain.Vault{}, domain.NotFound("vault not found")
	}
	return item, nil
}
func (r *httpVaultRepository) UpdateVault(_ context.Context, id string, patch app.VaultUpdateInput, clock domain.Clock) (domain.Vault, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.vaults[id]
	if !ok {
		return domain.Vault{}, domain.NotFound("vault not found")
	}
	if patch.DisplayName.Present && patch.DisplayName.Value != nil {
		item.DisplayName = *patch.DisplayName.Value
	}
	if patch.Metadata.Present {
		if patch.Metadata.Value == nil {
			item.Metadata = map[string]string{}
		} else {
			for key, value := range *patch.Metadata.Value {
				if value == nil {
					delete(item.Metadata, key)
				} else {
					item.Metadata[key] = *value
				}
			}
		}
	}
	item.UpdatedAt = clock.Now().UTC()
	r.vaults[id] = item
	return item, nil
}
func (r *httpVaultRepository) ListVaults(_ context.Context, query app.VaultListQuery) (app.VaultListPage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	page := app.VaultListPage{Vaults: make([]domain.Vault, 0, len(r.vaults))}
	for _, item := range r.vaults {
		if (query.IncludeArchived || item.ArchivedAt == nil) && resourceAfterBoundary(item.CreatedAt, item.ID, query.After) {
			page.Vaults = append(page.Vaults, item)
		}
	}
	sort.Slice(page.Vaults, func(i, j int) bool {
		return resourceNewer(page.Vaults[i].CreatedAt, page.Vaults[i].ID, page.Vaults[j].CreatedAt, page.Vaults[j].ID)
	})
	limit := query.Limit
	if limit <= 0 {
		limit = app.DefaultVaultListLimit
	}
	if len(page.Vaults) > limit {
		page.HasNext = true
		page.Vaults = page.Vaults[:limit]
	}
	return page, nil
}
func (r *httpVaultRepository) ArchiveVault(_ context.Context, id string, clock domain.Clock) (domain.Vault, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.vaults[id]
	if !ok {
		return domain.Vault{}, domain.NotFound("vault not found")
	}
	now := clock.Now().UTC()
	item.ArchivedAt, item.UpdatedAt = &now, now
	r.vaults[id] = item
	for credentialID, credential := range r.credentials {
		if credential.VaultID == id && credential.ArchivedAt == nil {
			credential.ArchivedAt, credential.UpdatedAt = &now, now
			credential.SecretEnvelope = nil
			r.credentials[credentialID] = credential
		}
	}
	return item, nil
}
func (r *httpVaultRepository) DeleteVault(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.vaults[id]; !ok {
		return domain.NotFound("vault not found")
	}
	delete(r.vaults, id)
	for credentialID, credential := range r.credentials {
		if credential.VaultID == id {
			delete(r.credentials, credentialID)
		}
	}
	return nil
}
func (r *httpVaultRepository) CreateCredential(_ context.Context, item domain.VaultCredential, _ int) (domain.VaultCredential, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.vaults[item.VaultID]; !ok {
		return domain.VaultCredential{}, domain.NotFound("vault not found")
	}
	r.credentials[item.ID] = item
	return item, nil
}
func (r *httpVaultRepository) GetCredential(_ context.Context, vaultID, credentialID string) (domain.VaultCredential, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.credentials[credentialID]
	if !ok || item.VaultID != vaultID {
		return domain.VaultCredential{}, domain.NotFound("credential not found")
	}
	return item, nil
}
func (r *httpVaultRepository) UpdateCredential(_ context.Context, vaultID, credentialID string, update func(domain.VaultCredential) (domain.VaultCredential, bool, error)) (domain.VaultCredential, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.credentials[credentialID]
	if !ok || item.VaultID != vaultID {
		return domain.VaultCredential{}, domain.NotFound("credential not found")
	}
	next, changed, err := update(item)
	if err != nil {
		return domain.VaultCredential{}, err
	}
	if changed {
		r.credentials[credentialID] = next
		item = next
	}
	return item, nil
}
func (r *httpVaultRepository) ListCredentials(_ context.Context, vaultID string, query app.CredentialListQuery) (app.CredentialListPage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.vaults[vaultID]; !ok {
		return app.CredentialListPage{}, domain.NotFound("vault not found")
	}
	page := app.CredentialListPage{Credentials: make([]domain.VaultCredential, 0)}
	for _, item := range r.credentials {
		if item.VaultID == vaultID && (query.IncludeArchived || item.ArchivedAt == nil) && resourceAfterBoundary(item.CreatedAt, item.ID, query.After) {
			page.Credentials = append(page.Credentials, item)
		}
	}
	sort.Slice(page.Credentials, func(i, j int) bool {
		return resourceNewer(page.Credentials[i].CreatedAt, page.Credentials[i].ID, page.Credentials[j].CreatedAt, page.Credentials[j].ID)
	})
	limit := query.Limit
	if limit <= 0 {
		limit = app.DefaultCredentialListLimit
	}
	if len(page.Credentials) > limit {
		page.HasNext = true
		page.Credentials = page.Credentials[:limit]
	}
	return page, nil
}

func resourceAfterBoundary(createdAt time.Time, id string, after *app.ResourcePageBoundary) bool {
	return after == nil || createdAt.Before(after.CreatedAt) || (createdAt.Equal(after.CreatedAt) && id < after.ID)
}

func resourceNewer(leftTime time.Time, leftID string, rightTime time.Time, rightID string) bool {
	return leftTime.After(rightTime) || (leftTime.Equal(rightTime) && leftID > rightID)
}
func (r *httpVaultRepository) ArchiveCredential(_ context.Context, vaultID, credentialID string, clock domain.Clock) (domain.VaultCredential, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.credentials[credentialID]
	if !ok || item.VaultID != vaultID {
		return domain.VaultCredential{}, domain.NotFound("credential not found")
	}
	now := clock.Now().UTC()
	item.ArchivedAt, item.UpdatedAt = &now, now
	item.SecretEnvelope = nil
	r.credentials[credentialID] = item
	return item, nil
}
func (r *httpVaultRepository) DeleteCredential(_ context.Context, vaultID, credentialID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.credentials[credentialID]
	if !ok || item.VaultID != vaultID {
		return domain.NotFound("credential not found")
	}
	delete(r.credentials, credentialID)
	return nil
}
