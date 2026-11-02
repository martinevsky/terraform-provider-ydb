package externaldatasource

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Real released/base and candidate provider processes consume the same legacy
// EDS + secret state. This is provider state compatibility, not server-version
// coverage. Supplying the baseline binary is an explicit local integration gate.
func TestLocalTerraformYDBLegacyProviderUpgradeDowngrade(t *testing.T) {
	baseline := os.Getenv("YDB_TEST_LEGACY_PROVIDER_BINARY")
	if baseline == "" {
		t.Skip("YDB_TEST_LEGACY_PROVIDER_BINARY is required")
	}
	tf := os.Getenv("TF_ACC_TERRAFORM_PATH")
	if tf == "" {
		tf, _ = exec.LookPath("terraform")
	}
	if tf == "" {
		t.Skip("Terraform CLI required")
	}
	require.FileExists(t, baseline)
	f := newWireDB(t)
	f.allowLegacySecret = true
	dir := t.TempDir()
	next := filepath.Join(dir, "terraform-provider-ydb-next")
	build := exec.Command("go", "build", "-buildvcs=false", "-o", next, "../../..")
	buildOutput, err := build.CombinedOutput()
	require.NoError(t, err, string(buildOutput))
	var out string
	plugins := filepath.Join(dir, "provider")
	require.NoError(t, os.MkdirAll(plugins, 0700))
	active := filepath.Join(plugins, "terraform-provider-ydb")
	cli := filepath.Join(dir, "terraform.tfrc")
	require.NoError(t, os.WriteFile(cli, []byte(fmt.Sprintf(`provider_installation {
 dev_overrides {
  "terraform.storage.ydb.tech/provider/ydb" = %q
 }
 direct {}
}
`, plugins)), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(fmt.Sprintf(`terraform {
 required_providers {
  ydb = { source = "terraform.storage.ydb.tech/provider/ydb" }
 }
}
provider "ydb" { token = "legacy-transport-fixture" }
resource "ydb_external_data_source" "legacy" {
 connection_string = "grpc://%s/?database=/owner"
 path = "eds"
 source_type = "Ydb"
 location = "legacy-target:2135"
 auth_method = "NONE"
 database_id = "legacy-target"
 use_tls = false
}
resource "ydb_secret" "legacy" {
 connection_string = "grpc://%s/?database=/owner"
 name = "legacy-secret"
 value = "legacy-public-fixture"
}
`, f.addr, f.addr)), 0600))
	switchProvider := func(binary string) { _ = os.Remove(active); require.NoError(t, os.Symlink(binary, active)) }
	run := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, tf, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "TF_CLI_CONFIG_FILE="+cli, "CHECKPOINT_DISABLE=1", "TF_IN_AUTOMATION=1", "TF_LOG=OFF", "TF_LOG_SDK=OFF")
		raw, err := cmd.CombinedOutput()
		return string(raw), err
	}
	snapshot := func() map[string]map[string]interface{} {
		raw, err := os.ReadFile(filepath.Join(dir, "terraform.tfstate"))
		require.NoError(t, err)
		require.False(t, strings.Contains(string(raw), "legacy-public-fixture"), "secret plaintext in state")
		var state struct {
			Resources []struct {
				Type      string `json:"type"`
				Instances []struct {
					Attributes map[string]interface{} `json:"attributes"`
				} `json:"instances"`
			} `json:"resources"`
		}
		require.NoError(t, json.Unmarshal(raw, &state))
		result := map[string]map[string]interface{}{}
		for _, r := range state.Resources {
			require.Len(t, r.Instances, 1)
			result[r.Type] = r.Instances[0].Attributes
		}
		require.Len(t, result, 2)
		return result
	}
	switchProvider(baseline)
	out, err = run("apply", "-auto-approve", "-no-color")
	require.NoError(t, err, out)
	original := snapshot()
	secretHash := original["ydb_secret"]["value"]
	require.NotEmpty(t, secretHash)
	out, err = run("plan", "-detailed-exitcode", "-no-color")
	require.NoError(t, err, out)
	switchProvider(next)
	out, err = run("plan", "-detailed-exitcode", "-no-color")
	require.NoError(t, err, out)
	out, err = run("apply", "-auto-approve", "-no-color")
	require.NoError(t, err, out)
	upgraded := snapshot()
	require.Equal(t, original["ydb_external_data_source"]["id"], upgraded["ydb_external_data_source"]["id"])
	require.Equal(t, secretHash, upgraded["ydb_secret"]["value"])
	switchProvider(baseline)
	out, err = run("plan", "-detailed-exitcode", "-no-color")
	require.NoError(t, err, out)
	out, err = run("apply", "-auto-approve", "-no-color")
	require.NoError(t, err, out)
	rolledBack := snapshot()
	require.Equal(t, original, rolledBack)
	out, err = run("destroy", "-auto-approve", "-no-color")
	require.NoError(t, err, out)
	f.mu.Lock()
	defer f.mu.Unlock()
	require.False(t, f.exists)
	require.Empty(t, f.secrets)
	for _, q := range f.queries {
		require.NotContains(t, q, "__tf_iam_")
	}
	t.Log("base f42c55d -> candidate -> base f42c55d: legacy NONE EDS + hashed secret, zero-diff plans, stable identities, destroy")
}
