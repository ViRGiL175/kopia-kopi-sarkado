package cli

import (
	"io"
	"path/filepath"
	"testing"
)

func TestUnsafeArgumentsAreRejectedBeforeKopia(t *testing.T) {
	root := t.TempDir()
	base := []string{"--source", root, "--config-file", filepath.Join(root, "test.config")}
	for _, extra := range [][]string{
		{"--apply"}, {"--max-passes=0"}, {"--batch-size=-1"}, {"--keep-latest=0"},
		{"--estimate-multiplier=NaN"}, {"--estimate-multiplier=+Inf"},
		{"--protect-tag=broken"}, {"--protect-tag==empty-key"}, {"--maintenance-mode=typo"},
		{"--safety-margin=99999PiB"}, {"unexpected"}, {"--password=never-log-this"},
	} {
		if _, ok := parseConfig(append(append([]string{}, base...), extra...), io.Discard); ok {
			t.Fatalf("accepted invalid arguments %q", extra)
		}
	}
	cfg, ok := parseConfig(base, io.Discard)
	if !ok || cfg.Apply {
		t.Fatal("preflight must require explicit write authorization")
	}
}
