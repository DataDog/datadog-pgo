package pgo

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/pprof/profile"
)

// noInlineTargets is the set of functions ApplyNoInlineHack is configured to
// rename when they appear as the leaf of a sample. It mirrors the list in
// noinline.go so this test stays in sync with what the hack actually targets.
var noInlineTargets = []string{grpcProcessDataFunc, runtimeGoparkFunc}

// TestMergedProfilePipeline exercises the same code path the datadog-pgo CLI
// drives against the gopgo endpoint: it packages the bundled testdata profile
// as the ZIP the endpoint returns, then runs
// ProfilesDownload.MergedProfile -> ApplyNoInlineHack -> Write, and asserts the
// result is a valid, deterministic .pgo file with the no-inline workaround
// applied.
//
// It needs no network access and no Datadog credentials.
func TestMergedProfilePipeline(t *testing.T) {
	profBytes, err := os.ReadFile(filepath.Join("testdata", "grpc-anon.pprof"))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}

	// gopgoEndpointZIP packages two copies of the test profile as the ZIP the
	// /api/unstable/profiles/gopgo endpoint returns. Each entry is a separate
	// pprof stream that MergedProfile parses and merges.
	gopgoEndpointZIP := func() []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for _, name := range []string{"profile-0.pprof", "profile-1.pprof"} {
			w, err := zw.Create(name)
			if err != nil {
				t.Fatalf("create zip entry %s: %v", name, err)
			}
			if _, err := w.Write(profBytes); err != nil {
				t.Fatalf("write zip entry %s: %v", name, err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatalf("close zip: %v", err)
		}
		return buf.Bytes()
	}

	// runPipeline mirrors the CLI's Fetch end-to-end for the merge/write stage.
	runPipeline := func(dst string) (n int64, samples int, sha string) {
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		dl := &ProfilesDownload{data: gopgoEndpointZIP()}
		mp, err := dl.MergedProfile(log)
		if err != nil {
			t.Fatalf("MergedProfile: %v", err)
		}
		if err := mp.ApplyNoInlineHack(); err != nil {
			t.Fatalf("ApplyNoInlineHack: %v", err)
		}
		n, err = mp.Write(dst)
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		samples = mp.Samples()
		data, err := os.ReadFile(dst)
		if err != nil {
			t.Fatalf("read %s: %v", dst, err)
		}
		sum := sha256.Sum256(data)
		sha = string(sum[:])
		return n, samples, sha
	}

	// leafTargets returns the subset of noInlineTargets that appear as the leaf
	// function of at least one sample in prof. ApplyNoInlineHack only renames
	// a target when it is a leaf, so the assertions below are conditioned on
	// this set rather than assuming a specific target is present. This must be
	// computed from the pre-hack profile, since afterwards the targets are
	// already renamed and no longer match.
	leafTargets := func(prof *profile.Profile) map[string]bool {
		targets := make(map[string]bool, len(noInlineTargets))
		for _, name := range noInlineTargets {
			targets[name] = false
		}
		for _, s := range prof.Sample {
			leaf, ok := leafLine(s)
			if !ok || leaf.Function == nil {
				continue
			}
			if _, want := targets[leaf.Function.Name]; want {
				targets[leaf.Function.Name] = true
			}
		}
		return targets
	}

	// Derive the expected renamed set from the raw testdata, before the hack.
	rawProfile, err := profile.ParseData(profBytes)
	if err != nil {
		t.Fatalf("parse raw testdata: %v", err)
	}
	targets := leafTargets(rawProfile)

	dir := t.TempDir()
	out1 := filepath.Join(dir, "default.pgo")
	n, samples, sha1 := runPipeline(out1)

	// The merge must have produced a non-trivial profile.
	if samples <= 0 {
		t.Fatalf("merged profile has no samples; got %d", samples)
	}
	if n <= 0 {
		t.Fatalf("wrote no bytes; got %d", n)
	}

	// The output must be a valid pprof the Go toolchain can consume.
	parsed, err := profile.Parse(bytes.NewReader(mustRead(t, out1)))
	if err != nil {
		t.Fatalf("output is not a parseable pprof: %v", err)
	}
	if len(parsed.Sample) != samples {
		t.Fatalf("parsed sample count %d != merged %d", len(parsed.Sample), samples)
	}

	// The no-inline workaround renames specific leaf functions to discourage
	// bad inlining decisions (see golang/go#65532). profile.Function objects
	// are shared across all references to the same function, so renaming a leaf
	// renames every occurrence of that function.
	//
	// Rather than assume a specific target is present in the testdata, derive
	// the expected set from the raw testdata: a target is expected to be
	// renamed iff it appears as a leaf of some sample.
	anyRenamed := false
	for name, isLeaf := range targets {
		if !isLeaf {
			continue
		}
		anyRenamed = true
		renamed := doNotInlinePrefix + name
		if !functionExists(parsed, renamed) {
			t.Errorf("no-inline workaround did not rename leaf %q to %q", name, renamed)
		}
		if functionExists(parsed, name) {
			t.Errorf("leaf %q was renamed but an unmodified %q still exists (Function not shared?)", name, name)
		}
	}
	// Guard against the test becoming vacuous: if the testdata no longer has
	// any of the no-inline targets as a leaf, the workaround is not exercised
	// at all and this test should fail loudly rather than silently pass.
	if !anyRenamed {
		t.Fatalf("testdata exercises none of the no-inline targets %v as a leaf; test is vacuous", noInlineTargets)
	}

	// The pipeline must be deterministic: a second run yields byte-identical
	// output. This guards against accidental non-determinism (map iteration
	// order, timestamps, etc.) that would break reproducible PGO builds.
	out2 := filepath.Join(dir, "default.pgo.again")
	_, samples2, sha2 := runPipeline(out2)
	if samples != samples2 {
		t.Errorf("non-deterministic sample count: %d vs %d", samples, samples2)
	}
	if sha1 != sha2 {
		t.Errorf("non-deterministic output: sha256 differs between runs")
	}
}

// functionExists reports whether prof contains a function with the exact name.
func functionExists(prof *profile.Profile, name string) bool {
	for _, f := range prof.Function {
		if f.Name == name {
			return true
		}
	}
	return false
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
