package supportbundle_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/distr-sh/distr/internal/supportbundle"
	"github.com/distr-sh/distr/internal/types"
	"github.com/google/uuid"
	. "github.com/onsi/gomega"
)

const (
	collectScriptHeredocStart = "<< 'DISTR_COLLECT_EOF'\n"
	collectScriptHeredocEnd   = "\nDISTR_COLLECT_EOF\n"
)

// The collector doing the actual work sits inside a quoted heredoc, so nothing parses it until a
// customer runs it on their host. Render both the branch with and the branch without custom scripts
// and check that each one is valid shell and carries the stable manifest identities.
func TestGenerateCollectScript_syntax(t *testing.T) {
	scriptID1 := uuid.New()
	scriptID2 := uuid.New()
	for name, scripts := range map[string][]types.SupportBundleCollectionScript{
		"without custom scripts": nil,
		"with custom scripts": {
			{ID: scriptID1, Name: "disk usage", Description: ptr("what is on disk"), Content: "df -h\n"},
			{ID: scriptID2, Name: "plain", Content: "echo hello\n"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			generation := uuid.New()
			script, err := supportbundle.GenerateCollectScript(supportbundle.CollectScriptParams{
				BaseURL:              "https://app.example.com",
				BundleID:             uuid.New(),
				BundleSecret:         "secret",
				CollectionGeneration: generation,
				EnvVars:              []types.SupportBundleCollectionEnvVar{{Name: "DB_HOST"}},
				Scripts:              scripts,
				LogTailLines:         100,
				ResourceMaxBytes:     1024,
				ScriptTimeout:        time.Minute,
			})
			g.Expect(err).ToNot(HaveOccurred())

			expectValidShellSyntax(t, g, script)
			inner := innerCollectScript(g, script)
			expectValidShellSyntax(t, g, inner)

			// The fixed generation and the stable identities of the required manifest must be present,
			// alongside the idempotent upload and resume endpoints.
			g.Expect(script).To(ContainSubstring(generation.String()))
			g.Expect(inner).To(ContainSubstring(`SYSINFO_KEY="system-info"`))
			g.Expect(inner).To(ContainSubstring(`"host-environment-variables"`))
			g.Expect(inner).To(ContainSubstring("${BASE_URL}/status?"))
			g.Expect(inner).To(ContainSubstring(`-F "stableKey=${_rp_key}"`))
			g.Expect(inner).To(ContainSubstring(`-F "sha256=${_rp_digest}"`))
			g.Expect(inner).To(ContainSubstring("${BASE_URL}/finalize?"))
			if len(scripts) > 0 {
				for _, id := range []uuid.UUID{scriptID1, scriptID2} {
					g.Expect(inner).To(ContainSubstring(id.String()))
				}
				g.Expect(inner).To(ContainSubstring(
					"custom-script:$(cat \"${_sbase}.id\")"))
			} else {
				g.Expect(inner).ToNot(ContainSubstring("custom-script:"))
			}
		})
	}
}

// TestCustomScriptStableKey documents the key format shared with the server-side manifest.
func TestCustomScriptStableKey(t *testing.T) {
	g := NewWithT(t)
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	g.Expect(supportbundle.CustomScriptStableKey(id)).
		To(Equal("custom-script:11111111-1111-1111-1111-111111111111"))
	g.Expect(supportbundle.SystemInfoStableKey).To(Equal("system-info"))
	g.Expect(supportbundle.HostEnvStableKey).To(Equal("host-environment-variables"))
}

func ptr[T any](v T) *T { return &v }

// innerCollectScript returns the collector that the outer script writes to a temp file.
func innerCollectScript(g *WithT, script string) string {
	start := strings.Index(script, collectScriptHeredocStart)
	g.Expect(start).ToNot(Equal(-1), "collect script is not written with the expected heredoc")
	body := script[start+len(collectScriptHeredocStart):]
	end := strings.Index(body, collectScriptHeredocEnd)
	g.Expect(end).ToNot(Equal(-1), "heredoc is not terminated")
	return body[:end]
}

func expectValidShellSyntax(t *testing.T, g *WithT, script string) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is not available")
	}

	file := filepath.Join(t.TempDir(), "collect-script.sh")
	g.Expect(os.WriteFile(file, []byte(script), 0o600)).To(Succeed())

	out, err := exec.Command(sh, "-n", file).CombinedOutput()
	g.Expect(err).ToNot(HaveOccurred(), "sh -n reported: %s", string(out))
}
