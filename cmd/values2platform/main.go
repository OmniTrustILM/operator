/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

// Command values2platform is a thin CLI wrapper over pkg/convert: it parses flags,
// reads the umbrella values.yaml, and prints the scaffolded otilm.com/v1alpha1
// Platform CR. All conversion logic lives in github.com/OmniTrustILM/operator/pkg/convert
// so it can be reused by any module without depending on package main.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/OmniTrustILM/operator/pkg/convert"
	"gopkg.in/yaml.v3"
)

// usage prints how to invoke the tool.
func usage() {
	fmt.Fprintf(os.Stderr, `values2platform — best-effort umbrella-chart values.yaml -> Platform CR converter.

Usage:
  values2platform -f <values.yaml> [-name <cr-name>] [-namespace <ns>] > platform.yaml
  values2platform <values.yaml>                                        > platform.yaml

It writes a scaffolded otilm.com/v1alpha1 Platform CR to stdout. Inline secrets are NEVER
copied: each becomes a Secret reference plus a "# TODO: create Secret" line in the header.
Unrecognized values are flagged "# UNMAPPED" and hand-migration items "# TODO(customization)".
Review the output before applying.

Flags:
`)
	flag.PrintDefaults()
}

// run reads the values file, converts it via pkg/convert, and writes the scaffolded CR to
// out. It is separated from main so it is testable and returns an error instead of exiting.
func run(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("values2platform", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = usage
	valuesPath := fs.String("f", "", "path to the umbrella chart values.yaml (or pass it as a positional arg)")
	name := fs.String("name", "ilm", "metadata.name for the generated Platform CR")
	namespace := fs.String("namespace", "ilm", "metadata.namespace for the CR (and the namespace the scaffolded Secrets are created in)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	path := *valuesPath
	if path == "" && fs.NArg() > 0 {
		path = fs.Arg(0)
	}
	if path == "" {
		usage()
		return fmt.Errorf("no values file given (use -f <path> or a positional arg)")
	}

	data, err := os.ReadFile(path) //nolint:gosec // CLI reads a user-named values file by design
	if err != nil {
		return fmt.Errorf("read values file %q: %w", path, err)
	}

	var values map[string]interface{}
	if err := yaml.Unmarshal(data, &values); err != nil {
		return fmt.Errorf("parse values YAML %q: %w", path, err)
	}
	if values == nil {
		values = map[string]interface{}{}
	}

	result := convert.Convert(values, *name, *namespace)
	rendered, err := result.Render()
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, rendered)
	return err
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
