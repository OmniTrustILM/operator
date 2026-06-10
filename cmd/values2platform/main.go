/*
Copyright (c) ILM.

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package main

import (
	"flag"
	"fmt"
	"io"
	"os"

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

// run reads the values file, converts it, and writes the scaffolded CR to out. It is
// separated from main so it is testable and returns an error instead of exiting.
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

	result := Convert(values, *name, *namespace)
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
