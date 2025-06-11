package main

import (
	"fmt"

	"github.com/google/pprof/profile"
)

const (
	grpcProcessDataFunc = "google.golang.org/grpc/internal/transport.(*loopyWriter).processData"
	runtimeGoparkFunc   = "runtime.gopark"
	doNotInlinePrefix   = "DO NOT INLINE: "
)

// ApplyNoInlineHack renames problematic functions in the profile to avoid bad
// inlining decisions that can have a large memory impact.
// See https://github.com/golang/go/issues/65532 for details.
//
// TODO: Delete this once it's fixed upstream.
func ApplyNoInlineHack(prof *profile.Profile) error {
	if err := renameNoInlineFuncs(prof, []string{grpcProcessDataFunc, runtimeGoparkFunc}); err != nil {
		return fmt.Errorf("noinline hack: %w", err)
	}
	return nil
}

func renameNoInlineFuncs(prof *profile.Profile, noInlineFuncs []string) error {
	for _, s := range prof.Sample {
		leaf, ok := leafLine(s)
		if ok && lineContainsAny(leaf, noInlineFuncs) {
			// There might be multiple samples that point to the same function.
			// But once we rename the function for the function for the first
			// time, it stops matching the noInlineFuncs list. So we don't end
			// up adding the prefix multiple times.
			leaf.Function.Name = doNotInlinePrefix + leaf.Function.Name
		}
	}
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
