package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thehatchcloud/msm/internal/servers"
)

func TestStatusJSON(t *testing.T) {
	f := newServerFixture(t)
	for _, n := range []string{"alpha", "beta", "gamma", "delta"} {
		f.mustRun(t, "server", "create", n)
	}
	for _, n := range []string{"alpha", "beta"} { // active
		if err := os.WriteFile(filepath.Join(f.root, n, "active"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.states["alpha"] = servers.Running // active and running
	// beta: active and stopped
	f.states["gamma"] = servers.Running  // inactive but running
	f.states["delta"] = servers.Occupied // inactive, name held by something else

	var all statusDocument
	decodeStrict(t, f.mustRun(t, "all", "status", "--format", "json"), &all)
	want := []serverStatus{
		{Name: "alpha", Active: true, State: "running", OK: true},
		{Name: "beta", Active: true, State: "stopped", OK: false},
		{Name: "delta", Active: false, State: "occupied", OK: false, Detail: "test"},
		{Name: "gamma", Active: false, State: "running", OK: false},
	}
	if all.SchemaVersion != 1 || !slices.Equal(all.Servers, want) {
		t.Fatalf("all status = %+v, want %+v", all, want)
	}

	var one statusDocument
	decodeStrict(t, f.mustRun(t, "alpha", "status", "--format", "json"), &one)
	if !slices.Equal(one.Servers, want[:1]) {
		t.Fatalf("alpha status = %+v", one)
	}
	var list statusDocument
	decodeStrict(t, f.mustRun(t, "server", "list", "--format", "json"), &list)
	if !slices.Equal(list.Servers, want) {
		t.Fatalf("server list = %+v", list)
	}

	code, out, errOut := f.run(t, "", "ghost", "status", "--format", "json")
	if code != ExitNotFound || out != "" || !strings.Contains(errOut, `no server with the name "ghost"`) {
		t.Errorf("ghost = %d %q %q", code, out, errOut)
	}
	// The ordinary text stays what it was.
	if out := f.mustRun(t, "beta", "status"); out != "Server \"beta\" is stopped.\n" {
		t.Errorf("text status = %q", out)
	}
	empty := newServerFixture(t)
	if out := empty.mustRun(t, "server", "list", "--format", "json"); out != "{\n  \"schema_version\": 1,\n  \"servers\": []\n}\n" {
		t.Errorf("empty list = %q", out)
	}
}

func TestVersionJSON(t *testing.T) {
	f := newServerFixture(t)
	var doc versionDocument
	decodeStrict(t, f.mustRun(t, "version", "--format", "json"), &doc)
	if doc != (versionDocument{SchemaVersion: 1, Version: "test"}) {
		t.Fatalf("doc = %+v", doc)
	}
}

// docs/schema/status-v1.json is the contract; the Go type must not drift
// from it in either direction.
func TestStatusSchemaMatchesTheDocument(t *testing.T) {
	data, err := os.ReadFile("../../docs/schema/status-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
		Defs       map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	var top struct {
		SchemaVersion struct {
			Const int `json:"const"`
		} `json:"schema_version"`
		Servers struct {
			Items struct {
				Required   []string                   `json:"required"`
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"items"`
		} `json:"servers"`
	}
	raw, _ := json.Marshal(schema.Properties)
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	if top.SchemaVersion.Const != SchemaVersion {
		t.Errorf("schema_version const = %d, SchemaVersion = %d", top.SchemaVersion.Const, SchemaVersion)
	}
	fields := func(v any) (all, required []string) {
		b, _ := json.Marshal(v)
		var m map[string]json.RawMessage
		_ = json.Unmarshal(b, &m)
		for k := range m {
			all = append(all, k)
		}
		slices.Sort(all)
		return all, nil
	}
	// A fully populated value shows every field the Go type can emit.
	gotAll, _ := fields(serverStatus{Name: "n", State: "s", Detail: "d"})
	var wantAll []string
	for k := range top.Servers.Items.Properties {
		wantAll = append(wantAll, k)
	}
	slices.Sort(wantAll)
	if !slices.Equal(gotAll, wantAll) {
		t.Errorf("serverStatus fields %v, schema properties %v", gotAll, wantAll)
	}
	// Required: the fields that are present when Detail is empty.
	gotReq, _ := fields(serverStatus{Name: "n", State: "s"})
	slices.Sort(top.Servers.Items.Required)
	if !slices.Equal(gotReq, top.Servers.Items.Required) {
		t.Errorf("always-present fields %v, schema required %v", gotReq, top.Servers.Items.Required)
	}
	docFields, _ := fields(statusDocument{})
	slices.Sort(schema.Required)
	if !slices.Equal(docFields, schema.Required) {
		t.Errorf("document fields %v, schema required %v", docFields, schema.Required)
	}
}
