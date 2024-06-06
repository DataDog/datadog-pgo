package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/pprof/profile"
	"github.com/stretchr/testify/require"
)

func TestApplyNoInlineHack(t *testing.T) {
	prof := loadTestProfile(t, "grpc-anon.pprof")
	require.Equal(t, 17, leafSamples(prof, grpcProcessDataFunc))
	require.NoError(t, ApplyNoInlineHack(prof))
	require.Equal(t, 0, leafSamples(prof, grpcProcessDataFunc))
	require.Equal(t, 17, leafSamples(prof, doNotInlinePrefix+grpcProcessDataFunc))
}

func loadTestProfile(t *testing.T, name string) *profile.Profile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	prof, err := profile.ParseData(data)
	require.NoError(t, err)
	return prof
}

func leafSamples(prof *profile.Profile, fn string) (count int) {
	for _, s := range prof.Sample {
		leaf, ok := leafLine(s)
		if ok && leaf.Function.Name == fn {
			count++
		}
	}
	return count
}
