package remote

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	tfe "github.com/hashicorp/go-tfe"
	"github.com/hashicorp/terraform/backend"
	"github.com/hashicorp/terraform/configs"
	"github.com/hashicorp/terraform/configs/configschema"
	"github.com/hashicorp/terraform/providers"
	"github.com/hashicorp/terraform/state/remote"
	"github.com/hashicorp/terraform/svchost"
	"github.com/hashicorp/terraform/svchost/auth"
	"github.com/hashicorp/terraform/svchost/disco"
	"github.com/hashicorp/terraform/terraform"
	"github.com/hashicorp/terraform/tfdiags"
	"github.com/mitchellh/cli"
	"github.com/zclconf/go-cty/cty"

	backendLocal "github.com/hashicorp/terraform/backend/local"
)

const (
	testCred = "test-auth-token"
)

var (
	tfeHost  = svchost.Hostname(defaultHostname)
	credsSrc = auth.StaticCredentialsSource(map[svchost.Hostname]map[string]interface{}{
		tfeHost: {"token": testCred},
	})
)

func testInput(t *testing.T, answers map[string]string) *mockInput {
	return &mockInput{answers: answers}
}

func testBackendDefault(t *testing.T) *Remote {
	obj := cty.ObjectVal(map[string]cty.Value{
		"hostname":     cty.NullVal(cty.String),
		"organization": cty.StringVal("hashicorp"),
		"token":        cty.NullVal(cty.String),
		"workspaces": cty.ObjectVal(map[string]cty.Value{
			"name":   cty.StringVal("prod"),
			"prefix": cty.NullVal(cty.String),
		}),
	})
	return testBackend(t, obj)
}

func testBackendNoDefault(t *testing.T) *Remote {
	obj := cty.ObjectVal(map[string]cty.Value{
		"hostname":     cty.NullVal(cty.String),
		"organization": cty.StringVal("hashicorp"),
		"token":        cty.NullVal(cty.String),
		"workspaces": cty.ObjectVal(map[string]cty.Value{
			"name":   cty.NullVal(cty.String),
			"prefix": cty.StringVal("my-app-"),
		}),
	})
	return testBackend(t, obj)
}

func testRemoteClient(t *testing.T) remote.Client {
	b := testBackendDefault(t)
	raw, err := b.StateMgr(backend.DefaultStateName)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	s := raw.(*remote.State)
	return s.Client
}

func testBackend(t *testing.T, obj cty.Value) *Remote {
	s := testServer(t)
	b := New(testDisco(s))

	// Configure the backend so the client is created.
	valDiags := b.ValidateConfig(obj)
	if len(valDiags) != 0 {
		t.Fatal(valDiags.ErrWithWarnings())
	}

	confDiags := b.Configure(obj)
	if len(confDiags) != 0 {
		t.Fatal(confDiags.ErrWithWarnings())
	}

	// Get a new mock client.
	mc := newMockClient()

	// Replace the services we use with our mock services.
	b.CLI = cli.NewMockUi()
	b.client.Applies = mc.Applies
	b.client.ConfigurationVersions = mc.ConfigurationVersions
	b.client.Organizations = mc.Organizations
	b.client.Plans = mc.Plans
	b.client.PolicyChecks = mc.PolicyChecks
	b.client.Runs = mc.Runs
	b.client.StateVersions = mc.StateVersions
	b.client.Workspaces = mc.Workspaces

	b.ShowDiagnostics = func(vals ...interface{}) {
		var diags tfdiags.Diagnostics
		for _, diag := range diags.Append(vals...) {
			b.CLI.Error(diag.Description().Summary)
		}
	}

	// Set local to a local test backend.
	b.local = testLocalBackend(t, b)

	ctx := context.Background()

	// Create the organization.
	_, err := b.client.Organizations.Create(ctx, tfe.OrganizationCreateOptions{
		Name: tfe.String(b.organization),
	})
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	// Create the default workspace if required.
	if b.workspace != "" {
		_, err = b.client.Workspaces.Create(ctx, b.organization, tfe.WorkspaceCreateOptions{
			Name: tfe.String(b.workspace),
		})
		if err != nil {
			t.Fatalf("error: %v", err)
		}
	}

	return b
}

func testLocalBackend(t *testing.T, remote *Remote) backend.Enhanced {
	b := backendLocal.NewWithBackend(remote)

	b.CLI = remote.CLI
	b.ShowDiagnostics = remote.ShowDiagnostics

	// Add a test provider to the local backend.
	p := backendLocal.TestLocalProvider(t, b, "null", &terraform.ProviderSchema{
		ResourceTypes: map[string]*configschema.Block{
			"null_resource": {
				Attributes: map[string]*configschema.Attribute{
					"id": {Type: cty.String, Computed: true},
				},
			},
		},
	})
	p.ApplyResourceChangeResponse = providers.ApplyResourceChangeResponse{NewState: cty.ObjectVal(map[string]cty.Value{
		"id": cty.StringVal("yes"),
	})}

	return b
}

// testServer returns a *httptest.Server used for local testing.
func testServer(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()

	// Respond to service discovery calls.
	mux.HandleFunc("/well-known/terraform.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"tfe.v2":"/api/v2/"}`)
	})

	// Respond to the initial query to read the organization settings.
	mux.HandleFunc("/api/v2/organizations/hashicorp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.api+json")
		io.WriteString(w, `{
  "data": {
    "id": "hashicorp",
    "type": "organizations",
    "attributes": {
      "name": "hashicorp",
      "created-at": "2017-09-07T14:34:40.492Z",
      "email": "user@example.com",
      "collaborator-auth-policy": "password",
      "enterprise-plan": "premium",
      "permissions": {
        "can-update": true,
        "can-destroy": true,
        "can-create-team": true,
        "can-create-workspace": true,
        "can-update-oauth": true,
        "can-update-api-token": true,
        "can-update-sentinel": true,
        "can-traverse": true,
        "can-create-workspace-migration": true
      }
    }
  }
}`)
	})

	// All tests that are assumed to pass will use the hashicorp organization,
	// so for all other organization requests we will return a 404.
	mux.HandleFunc("/api/v2/organizations/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		io.WriteString(w, `{
  "errors": [
    {
      "status": "404",
      "title": "not found"
    }
  ]
}`)
	})

	return httptest.NewServer(mux)
}

// testDisco returns a *disco.Disco mapping app.terraform.io and
// localhost to a local test server.
func testDisco(s *httptest.Server) *disco.Disco {
	services := map[string]interface{}{
		"tfe.v2": fmt.Sprintf("%s/api/v2/", s.URL),
	}
	d := disco.NewWithCredentialsSource(credsSrc)

	d.ForceHostServices(svchost.Hostname(defaultHostname), services)
	d.ForceHostServices(svchost.Hostname("localhost"), services)
	return d
}

type unparsedVariableValue struct {
	value  string
	source terraform.ValueSourceType
}

func (v *unparsedVariableValue) ParseVariableValue(mode configs.VariableParsingMode) (*terraform.InputValue, tfdiags.Diagnostics) {
	return &terraform.InputValue{
		Value:      cty.StringVal(v.value),
		SourceType: v.source,
	}, tfdiags.Diagnostics{}
}

// testVariable returns a backend.UnparsedVariableValue used for testing.
func testVariables(s terraform.ValueSourceType, vs ...string) map[string]backend.UnparsedVariableValue {
	vars := make(map[string]backend.UnparsedVariableValue, len(vs))
	for _, v := range vs {
		vars[v] = &unparsedVariableValue{
			value:  v,
			source: s,
		}
	}
	return vars
}
