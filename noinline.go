package main

import (
	"fmt"

	"github.com/google/pprof/profile"
)

const grpcProcessDataFunc = "google.golang.org/grpc/internal/transport.(*loopyWriter).processData"

// ApplyNoInlineHack removes problematic samples from the profile to avoid bad
// inlining decisions that can have a large memory impact.
// See https://github.com/golang/go/issues/65532 for details.
//
// TODO: Delete this once it's fixed upstream.
func ApplyNoInlineHack(prof *profile.Profile) error {
	if err := removeLeafSamples(prof, []string{grpcProcessDataFunc}); err != nil {
		return fmt.Errorf("noinline hack: %w", err)
	}
	return nil
}

func removeLeafSamples(prof *profile.Profile, funcs []string) error {
	newSamples := make([]*profile.Sample, 0, len(prof.Sample))
	for _, s := range prof.Sample {
		leaf, ok := leafLine(s)
		if ok && lineContainsAny(leaf, funcs) {
			continue
		}
		newSamples = append(newSamples, s)
	}
	prof.Sample = newSamples
	return nil
}

func leafLine(s *profile.Sample) (profile.Line, bool) {
	if len(s.Location) == 0 || len(s.Location[0].Line) == 0 {
		return profile.Line{}, false
	}
	return s.Location[0].Line[0], true
}

func lineContainsAny(leaf profile.Line, funcs []string) bool {
	for _, fn := range funcs {
		if leaf.Function.Name == fn {
			return true
		}
	}
	return false
}
