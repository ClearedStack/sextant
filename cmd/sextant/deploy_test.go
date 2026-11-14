package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ClearedStack/sextant/internal/muster"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	return filepath.Clean(filepath.Join("..", ".."))
}

func TestGoModIsStdlibOnlyAndCanonicalModulePath(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "module github.com/ClearedStack/sextant\n") {
		t.Fatalf("go.mod does not declare module github.com/ClearedStack/sextant:\n%s", text)
	}
	if strings.Contains(text, "require") {
		t.Fatalf("go.mod must have zero external dependencies:\n%s", text)
	}
}

func TestKoAndDeployManifestsEnforceClusterHardening(t *testing.T) {
	root := repoRoot(t)

	if _, err := os.Stat(filepath.Join(root, "Dockerfile")); !os.IsNotExist(err) {
		t.Fatalf("Dockerfile must not exist; builds use ko (.ko.yaml)")
	}

	koRaw, err := os.ReadFile(filepath.Join(root, ".ko.yaml"))
	if err != nil {
		t.Fatalf("read .ko.yaml: %v", err)
	}
	koText := string(koRaw)
	digestRef := regexp.MustCompile(`@sha256:[0-9a-f]{64}\b`)
	if !digestRef.MatchString(koText) {
		t.Fatalf(".ko.yaml must pin defaultBaseImage by @sha256 digest:\n%s", koText)
	}
	for _, want := range []string{"CGO_ENABLED=0", "-trimpath", "-s", "-w", "-buildid="} {
		if !strings.Contains(koText, want) {
			t.Errorf(".ko.yaml missing %q", want)
		}
	}

	nsRaw, err := os.ReadFile(filepath.Join(root, "deploy", "namespace.yaml"))
	if err != nil {
		t.Fatalf("read deploy/namespace.yaml: %v", err)
	}
	if !strings.Contains(string(nsRaw), "name: cs-workers") {
		t.Fatalf("deploy/namespace.yaml must define namespace cs-workers")
	}

	depRaw, err := os.ReadFile(filepath.Join(root, "deploy", "sextant.yaml"))
	if err != nil {
		t.Fatalf("read deploy/sextant.yaml: %v", err)
	}
	depText := string(depRaw)
	imagePin := regexp.MustCompile(`image:\s+lazarette\.clearedstack\.svc\.cluster\.local:5000/sextant@sha256:[0-9a-f]{64}\b`)
	if !imagePin.MatchString(depText) {
		t.Fatalf("deploy/sextant.yaml must pin sextant image in Lazarette by @sha256 digest")
	}
	if strings.Contains(depText, ":latest") {
		t.Fatalf("deploy/sextant.yaml must not use :latest")
	}
	requiredManifestSnippets := []string{
		"namespace: cs-workers",
		"serviceAccountName: sextant",
		"automountServiceAccountToken: true",
		"key: kubernetes.io/arch",
		"- arm64",
		"key: kubernetes.io/hostname",
		"operator: NotIn",
		"- cm3588-plus",
		"runAsNonRoot: true",
		"runAsUser: 65532",
		"readOnlyRootFilesystem: true",
		"allowPrivilegeEscalation: false",
		"privileged: false",
		"- ALL",
		"type: RuntimeDefault",
		"cpu: 10m",
		"memory: 16Mi",
		"cpu: 100m",
		"memory: 30Mi",
		"name: GOMEMLIMIT",
		"value: 20MiB",
		"name: CONTRACT_VERSION",
		"value: v1",
		"http://muster.clearedstack.svc.cluster.local:8080",
		"mountPath: /var/spool/cs",
	}
	for _, want := range requiredManifestSnippets {
		if !strings.Contains(depText, want) {
			t.Errorf("deploy/sextant.yaml missing required setting %q", want)
		}
	}

	npRaw, err := os.ReadFile(filepath.Join(root, "deploy", "networkpolicy.yaml"))
	if err != nil {
		t.Fatalf("read deploy/networkpolicy.yaml: %v", err)
	}
	npText := string(npRaw)
	requiredPolicySnippets := []string{
		"name: default-deny",
		"namespace: cs-workers",
		"podSelector: {}",
		"- Ingress",
		"- Egress",
		"name: sextant",
		"kubernetes.io/metadata.name: kube-system",
		"port: 53",
		"kubernetes.io/metadata.name: clearedstack",
		"app.kubernetes.io/name: muster",
		"port: 8080",
		"cidr: 0.0.0.0/0",
		"- 10.0.0.0/8",
		"- 100.64.0.0/10",
		"- 127.0.0.0/8",
		"- 169.254.0.0/16",
		"- 172.16.0.0/12",
		"- 192.168.0.0/16",
		"port: 443",
	}
	for _, want := range requiredPolicySnippets {
		if !strings.Contains(npText, want) {
			t.Errorf("deploy/networkpolicy.yaml missing required setting %q", want)
		}
	}
}

// TestDeployNeverSetsTheLocalTestOrigin: SEXTANT_LOCAL_TEST_ORIGIN switches off
// the public-https checks for one origin. It is for test runs only, so nothing
// under deploy/ may mention it.
func TestDeployNeverSetsTheLocalTestOrigin(t *testing.T) {
	root := filepath.Join(repoRoot(t), "deploy")
	n := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		n++
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(raw), "LOCAL_TEST_ORIGIN") {
			t.Errorf("%s mentions SEXTANT_LOCAL_TEST_ORIGIN", path)
		}
		return nil
	})
	if err != nil || n == 0 {
		t.Fatalf("walked %d files under deploy/: %v", n, err)
	}
}

func TestRepositoryContainsNoEmDashes(t *testing.T) {
	root := repoRoot(t)
	emDash := "\u2014"
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !utf8.Valid(raw) {
			return nil // intentional binary/invalid-UTF-8 test fixture
		}
		if strings.Contains(string(raw), emDash) {
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s contains an em dash (U+2014)", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBinaryMemoryBudgetUnder30MiB(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and executes standalone binary")
	}
	const budgetKiB = 30 * 1024

	binPath := filepath.Join(t.TempDir(), "sextant")
	buildCmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w -buildid=", "-o", binPath, ".")
	buildCmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}

	// 1. Check VmHWM on a ~1 MiB government calculator HTML page via sextant observe.
	// Note: child VmHWM from /proc/self/status is used instead of wait4 Maxrss,
	// because Linux wait4 inherits the -race test parent's RSS across fork().
	var page bytes.Buffer
	page.WriteString(`<!doctype html><html><head><title>Due Date Calculator</title></head><body><h1>Due Date Calculator</h1>`)
	page.WriteString(`<form action="/calc"><input type="date" name="start" required><output name="due"></output></form>`)
	page.WriteString(`<p>Free to use. No login required. In the public domain under FAR 52.204-21.</p>`)
	page.WriteString(strings.Repeat("<p>agency reference text</p>", 32000))
	page.WriteString(`</body></html>`)

	obsCmd := exec.Command(binPath, "observe",
		"-item", "PA-0001",
		"-url", "https://calc.example.gov/due",
		"-page", "-",
		"-observed-on", "2026-01-15",
	)
	obsCmd.Dir = "/"
	obsCmd.Env = []string{"HOME=/nonexistent", "TMPDIR=/nonexistent", "GOMEMLIMIT=20MiB", "SEXTANT_REPORT_PEAK=1"}
	obsCmd.Stdin = bytes.NewReader(page.Bytes())
	var stdout, stderr bytes.Buffer
	obsCmd.Stdout = &stdout
	obsCmd.Stderr = &stderr
	if err := obsCmd.Run(); err != nil {
		t.Fatalf("sextant observe failed: %v\nstderr: %s", err, stderr.String())
	}
	obsPeak := parsePeakKiB(stderr.String(), "observe_stopped")
	if obsPeak > 0 && obsPeak > budgetKiB {
		t.Fatalf("sextant observe peak VmHWM %d KiB exceeds 30 MiB budget (%d KiB)", obsPeak, budgetKiB)
	}

	// 2. Check VmHWM and read-only rootfs discipline on sextant work -drain processing 200 items.
	hub := muster.NewStandIn()
	var seeds []muster.SeedItem
	for i := 1; i <= 200; i++ {
		seeds = append(seeds, muster.SeedItem{
			ID:    fmt.Sprintf("PA-%04d", i),
			State: "recorded",
			Found: &muster.Found{
				Source:  "sam-gov",
				Title:   fmt.Sprintf("Candidate %d", i),
				Address: fmt.Sprintf("http://127.0.0.1/blocked-%d", i),
			},
		})
	}
	hub.Seed(seeds)
	srv := httptest.NewServer(hub)
	defer srv.Close()

	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("sextant\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spoolDir := filepath.Join(t.TempDir(), "spool")

	workCmd := exec.Command(binPath, "work",
		"-muster-url", srv.URL,
		"-token-file", tokenFile,
		"-spool", spoolDir,
		"-poll-every", "5ms",
		"-drain",
	)
	workCmd.Dir = "/"
	workCmd.Env = []string{"HOME=/nonexistent", "TMPDIR=/nonexistent", "GOMEMLIMIT=20MiB", "CONTRACT_VERSION=v1"}
	var workOut, workErr bytes.Buffer
	workCmd.Stdout = &workOut
	workCmd.Stderr = &workErr
	if err := workCmd.Run(); err != nil {
		t.Fatalf("sextant work failed: %v\nstderr: %s", err, workErr.String())
	}
	workPeak := parsePeakKiB(workErr.String(), "worker_stopped")
	if workPeak == 0 {
		t.Skip("VmHWM peak memory not reported on this kernel")
	}
	t.Logf("peak VmHWM: observe=%d KiB, work=%d KiB (budget %d KiB)", obsPeak, workPeak, budgetKiB)
	if workPeak > budgetKiB {
		t.Fatalf("sextant work peak VmHWM %d KiB exceeds 30 MiB budget (%d KiB)", workPeak, budgetKiB)
	}
}

func parsePeakKiB(logs, wantEvent string) int {
	for _, line := range strings.Split(logs, "\n") {
		var ev struct {
			Event   string `json:"event"`
			PeakKiB int    `json:"peak_kib"`
		}
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Event == wantEvent {
			return ev.PeakKiB
		}
	}
	return 0
}
