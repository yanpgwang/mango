package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestEnvironments_DefaultSelfHostedWireShape(t *testing.T) {
	srv := newTestServer(t)
	rec := do(srv, http.MethodPost, "/v1/environments", `{"name":"default"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body)
	}
	var environment map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &environment); err != nil {
		t.Fatal(err)
	}
	if environment["description"] != "" {
		t.Fatalf("description = %#v", environment["description"])
	}
	if metadata, ok := environment["metadata"].(map[string]any); !ok || len(metadata) != 0 {
		t.Fatalf("metadata = %#v", environment["metadata"])
	}
	config, _ := environment["config"].(map[string]any)
	if len(config) != 1 || config["type"] != "self_hosted" {
		t.Fatalf("config = %#v", config)
	}
}

func TestEnvironments_LimitedNetworkingRoundTrip(t *testing.T) {
	srv := newTestServer(t)
	created := do(srv, http.MethodPost, "/v1/environments", `{
		"name":"limited",
		"config":{"type":"cloud","networking":{
			"type":"limited","allow_mcp_servers":true,
			"allowed_hosts":["api.example.com","*.assets.example.com"]
		}}
	}`)
	if created.Code != http.StatusOK {
		t.Fatalf("create status = %d: %s", created.Code, created.Body)
	}
	body := decodeBody(t, created.Body.Bytes())
	networking := body["config"].(map[string]any)["networking"].(map[string]any)
	if networking["type"] != "limited" || networking["allow_mcp_servers"] != true ||
		networking["allow_package_managers"] != false ||
		len(networking["allowed_hosts"].([]any)) != 2 {
		t.Fatalf("created networking = %#v", networking)
	}

	updated := do(srv, http.MethodPost, "/v1/environments/"+body["id"].(string), `{
		"config":{"type":"cloud","networking":{
			"type":"limited","allowed_hosts":["next.example.com"]
		}}
	}`)
	if updated.Code != http.StatusOK {
		t.Fatalf("update status = %d: %s", updated.Code, updated.Body)
	}
	networking = decodeBody(t, updated.Body.Bytes())["config"].(map[string]any)["networking"].(map[string]any)
	if networking["allow_mcp_servers"] != true ||
		networking["allowed_hosts"].([]any)[0] != "next.example.com" {
		t.Fatalf("updated networking = %#v", networking)
	}
}

func TestEnvironments_PackagesRoundTrip(t *testing.T) {
	srv := newTestServer(t)
	created := do(srv, http.MethodPost, "/v1/environments", `{
		"name":"packages",
		"config":{"type":"cloud","packages":{"apt":["git"],"pip":["httpx==0.28.1"]}}
	}`)
	if created.Code != http.StatusOK {
		t.Fatalf("create status = %d: %s", created.Code, created.Body)
	}
	body := decodeBody(t, created.Body.Bytes())
	packages := body["config"].(map[string]any)["packages"].(map[string]any)
	if packages["type"] != "packages" || packages["apt"].([]any)[0] != "git" ||
		packages["pip"].([]any)[0] != "httpx==0.28.1" || len(packages["npm"].([]any)) != 0 {
		t.Fatalf("created packages = %#v", packages)
	}

	id := body["id"].(string)
	updated := do(srv, http.MethodPost, "/v1/environments/"+id,
		`{"config":{"type":"cloud","packages":{"npm":["typescript@5.9.2"]}}}`)
	if updated.Code != http.StatusOK {
		t.Fatalf("update status = %d: %s", updated.Code, updated.Body)
	}
	packages = decodeBody(t, updated.Body.Bytes())["config"].(map[string]any)["packages"].(map[string]any)
	if packages["npm"].([]any)[0] != "typescript@5.9.2" || len(packages["pip"].([]any)) != 0 {
		t.Fatalf("updated packages = %#v", packages)
	}
}

func TestEnvironments_AcceptsExplicitCloudDefaults(t *testing.T) {
	srv := newTestServer(t)
	rec := do(srv, http.MethodPost, "/v1/environments", `{
		"name":"explicit defaults",
		"config":{
			"type":"cloud",
			"networking":{"type":"unrestricted"},
			"packages":{"type":"packages","apt":[],"cargo":[],"gem":[],"go":[],"npm":[],"pip":[]}
		}
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body)
	}
	config := decodeBody(t, rec.Body.Bytes())["config"].(map[string]any)
	if config["networking"].(map[string]any)["type"] != "unrestricted" {
		t.Fatalf("networking = %#v", config["networking"])
	}
}

func TestEnvironments_ResourceFieldsRoundTrip(t *testing.T) {
	srv := newTestServer(t)
	rec := do(srv, http.MethodPost, "/v1/environments", `{
		"name":"local",
		"description":"developer laptop",
		"metadata":{"team":"sdk"},
		"scope":"account",
		"config":{"type":"self_hosted"}
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body)
	}
	created := decodeBody(t, rec.Body.Bytes())
	if created["description"] != "developer laptop" || created["scope"] != "account" {
		t.Fatalf("created environment = %#v", created)
	}
	metadata, _ := created["metadata"].(map[string]any)
	if metadata["team"] != "sdk" {
		t.Fatalf("created metadata = %#v", metadata)
	}
	config, _ := created["config"].(map[string]any)
	if config["type"] != "self_hosted" {
		t.Fatalf("created config = %#v", config)
	}

	id := created["id"].(string)
	got := do(srv, http.MethodGet, "/v1/environments/"+id, "")
	if got.Code != http.StatusOK {
		t.Fatalf("get status = %d: %s", got.Code, got.Body)
	}
	if body := decodeBody(t, got.Body.Bytes()); body["description"] != "developer laptop" ||
		body["scope"] != "account" || body["metadata"].(map[string]any)["team"] != "sdk" {
		t.Fatalf("retrieved environment = %#v", body)
	}
	archived := do(srv, http.MethodPost, "/v1/environments/"+id+"/archive", `{}`)
	if archived.Code != http.StatusOK {
		t.Fatalf("archive status = %d: %s", archived.Code, archived.Body)
	}
	archivedBody := decodeBody(t, archived.Body.Bytes())
	if archivedBody["archived_at"] == nil || archivedBody["scope"] != "account" ||
		archivedBody["metadata"].(map[string]any)["team"] != "sdk" {
		t.Fatalf("archived environment = %#v", archivedBody)
	}
}

func TestEnvironments_RejectsMalformedOptionalFields(t *testing.T) {
	srv := newTestServer(t)
	for _, body := range []string{
		`{"name":"bad","description":null}`,
		`{"name":"bad","description":42}`,
		`{"name":"bad","metadata":null}`,
		`{"name":"bad","metadata":[]}`,
		`{"name":"bad","metadata":{"team":1}}`,
		`{"name":"bad","scope":null}`,
		`{"name":"bad","scope":"workspace","config":{"type":"self_hosted"}}`,
		`{"name":"bad","scope":"account","config":{"type":"cloud"}}`,
		`{"name":"bad","config":null}`,
		`{"name":"bad","config":[]}`,
		`{"name":"bad","config":{"type":"cloud","networking":null}}`,
		`{"name":"bad","config":{"type":"cloud","networking":{"type":"unrestricted","future":true}}}`,
		`{"name":"bad","config":{"type":"cloud","networking":{"type":"limited","allowed_hosts":["https://example.com"]}}}`,
		`{"name":"bad","config":{"type":"cloud","networking":{"type":"limited","allowed_hosts":["example.com:443"]}}}`,
		`{"name":"bad","config":{"type":"cloud","packages":{"apt":null}}}`,
		`{"name":"bad","config":{"type":"cloud","packages":{"pip":[1]}}}`,
	} {
		rec := do(srv, http.MethodPost, "/v1/environments", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s status = %d, want 400: %s", body, rec.Code, rec.Body)
		}
	}
}

func TestEnvironments_UpdateRoundTrip(t *testing.T) {
	srv := newTestServer(t)
	created := do(srv, http.MethodPost, "/v1/environments", `{
		"name":"local","description":"old","metadata":{"keep":"old","drop":"value"},
		"scope":"account","config":{"type":"self_hosted"}
	}`)
	if created.Code != http.StatusOK {
		t.Fatalf("create status = %d: %s", created.Code, created.Body)
	}
	id := decodeBody(t, created.Body.Bytes())["id"].(string)

	updated := do(srv, http.MethodPost, "/v1/environments/"+id, `{
		"name":"renamed","description":"new","metadata":{
			"keep":"updated","drop":null,"empty_delete":""
		},"scope":"organization","config":{"type":"self_hosted"}
	}`)
	if updated.Code != http.StatusOK {
		t.Fatalf("update status = %d: %s", updated.Code, updated.Body)
	}
	body := decodeBody(t, updated.Body.Bytes())
	if body["name"] != "renamed" || body["description"] != "new" ||
		body["scope"] != "organization" {
		t.Fatalf("updated environment = %#v", body)
	}
	metadata := body["metadata"].(map[string]any)
	if len(metadata) != 1 || metadata["keep"] != "updated" {
		t.Fatalf("updated metadata = %#v", metadata)
	}

	got := do(srv, http.MethodGet, "/v1/environments/"+id, "")
	if got.Code != http.StatusOK || decodeBody(t, got.Body.Bytes())["name"] != "renamed" {
		t.Fatalf("persisted update status = %d: %s", got.Code, got.Body)
	}
}

func TestEnvironments_UpdateRejectsMalformedFields(t *testing.T) {
	srv := newTestServer(t)
	created := do(srv, http.MethodPost, "/v1/environments", `{"name":"stable"}`)
	if created.Code != http.StatusOK {
		t.Fatalf("create status = %d: %s", created.Code, created.Body)
	}
	id := decodeBody(t, created.Body.Bytes())["id"].(string)

	for _, body := range []string{
		`{"name":null}`,
		`{"description":null}`,
		`{"metadata":null}`,
		`{"metadata":{"bad":1}}`,
		`{"scope":null}`,
		`{"scope":""}`,
		`{"config":null}`,
		`{"config":{}}`,
		`{"config":{"type":"cloud","networking":null}}`,
		`{"config":{"type":"cloud","networking":{"type":"limited","allowed_hosts":["https://example.com"]}}}`,
		`{"config":{"type":"cloud","packages":null}}`,
		`{"future":true}`,
	} {
		rec := do(srv, http.MethodPost, "/v1/environments/"+id, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s status = %d, want 400: %s", body, rec.Code, rec.Body)
		}
	}
	got := do(srv, http.MethodGet, "/v1/environments/"+id, "")
	if got.Code != http.StatusOK || decodeBody(t, got.Body.Bytes())["name"] != "stable" {
		t.Fatalf("rejected update mutated environment: %d %s", got.Code, got.Body)
	}
}
