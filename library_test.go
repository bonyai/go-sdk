package bonya

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLibraryWire(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotBody = nil
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/templates":
			_, _ = w.Write([]byte(`{"id":"tpl-1","name":"n","revision":1,"config":{"image":"alpine"}}`))
		case "/v1/secret-sets":
			_, _ = w.Write([]byte(`{"id":"ss-1","name":"app","revision":1,"keys":["A"]}`))
		case "/v1/sandbox/gpu-options":
			_, _ = w.Write([]byte(`{"gpu_options":[{"type":"a100","count":1}]}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	c, err := NewClient(WithAPIKey("byk_test"), WithEndpoint(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	tpl, err := c.CreateTemplate(ctx, TemplateWrite{Name: "n", Config: &TemplateConfig{Image: "alpine", MemoryMiB: 512}})
	if err != nil || tpl.ID != "tpl-1" || tpl.Config.Image != "alpine" {
		t.Fatalf("template: %+v %v", tpl, err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/templates" {
		t.Fatalf("got %s %s", gotMethod, gotPath)
	}
	if cfg := gotBody["config"].(map[string]any); cfg["memory_mib"] != float64(512) {
		t.Fatalf("config body: %v", gotBody)
	}

	ss, err := c.CreateSecretSet(ctx, SecretSetWrite{Name: "app", Values: map[string]string{"A": "1"}})
	if err != nil || ss.Keys[0] != "A" {
		t.Fatalf("secret set: %+v %v", ss, err)
	}
	if gotBody["name"] != "app" || gotBody["values"].(map[string]any)["A"] != "1" {
		t.Fatalf("secret set body: %v", gotBody)
	}

	opts, err := c.ListGPUOptions(ctx)
	if err != nil || len(opts) != 1 || opts[0].Type != "a100" {
		t.Fatalf("gpu: %+v %v", opts, err)
	}

	if _, err := c.CreateTemplate(ctx, TemplateWrite{}); err == nil {
		t.Fatal("expected an error for a template without a name")
	}
}
