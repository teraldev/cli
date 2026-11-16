package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/spf13/cobra"

	"github.com/teraldev/cli/internal/schema"
)

const starterConfig = `#:schema ` + schema.URL + `
# Teral reads this file for each deployment. Keys you set here override the
# dashboard for that deployment; keys you leave out keep their dashboard
# values. Docs: https://docs.teral.dev/configure/teral-toml

[build]
# "dockerfile" builds a Dockerfile; "railpack" detects your language and
# builds without one.
# builder = "dockerfile"
# dockerfile = "Dockerfile"   # relative to the app's root directory
# command = "npm run build"   # Railpack only

[deploy]
# start_command = ["node", "server.js"]   # exec form; [] uses the image's command
# port = 8080
# protocol = "http1"          # or "h2c"
# shutdown_signal = "SIGTERM"

# [deploy.healthcheck]
# path = "/healthz"
# method = "GET"
# interval = "10s"
# timeout = "5s"
# failure_threshold = 3
# initial_delay = "0s"

# Per-environment overrides, merged over [build] and [deploy] key by key.
# [environments.preview.deploy]
# port = 3000
`

func newConfigCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Create and validate teral.toml",
		Long: `teral.toml configures an app's build and runtime from its repository, overriding
the dashboard settings for each deployment. It lives in the app's root
directory. Docs: https://docs.teral.dev/configure/teral-toml`,
	}

	var force bool
	initCmd := &cobra.Command{
		Use:   "init [directory]",
		Short: "Write a starter teral.toml",
		Long: `Write a commented teral.toml. Every setting starts commented out, so the file
changes nothing until you uncomment a line.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			if info, err := os.Stat(dir); err != nil || !info.IsDir() {
				return &UserError{Message: fmt.Sprintf("%s is not a directory.", dir)}
			}
			target := filepath.Join(dir, "teral.toml")
			flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
			if !force {
				flags |= os.O_EXCL
			}
			f, err := os.OpenFile(target, flags, 0o644)
			if errors.Is(err, os.ErrExist) {
				return &UserError{Message: target + " already exists.", Hint: "Pass --force to replace it."}
			}
			if err != nil {
				return err
			}
			if _, err := f.WriteString(starterConfig); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			a.IO.Success("Wrote %s", target)
			a.IO.Hint("Uncomment what you need, then check it with `teral config validate`.")
			return nil
		},
	}
	initCmd.Flags().BoolVar(&force, "force", false, "Replace an existing teral.toml")
	cmd.AddCommand(initCmd)

	cmd.AddCommand(&cobra.Command{
		Use:   "validate [file]",
		Short: "Check teral.toml against its schema",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			file := "teral.toml"
			if len(args) == 1 {
				file = args[0]
				if info, err := os.Stat(file); err == nil && info.IsDir() {
					file = filepath.Join(file, "teral.toml")
				}
			}
			problems, err := validateConfig(file)
			if err != nil {
				return err
			}
			if a.IO.JSON {
				return a.IO.PrintJSON(map[string]any{"file": file, "valid": len(problems) == 0, "problems": problems})
			}
			if len(problems) == 0 {
				a.IO.Success("%s is valid", file)
				return nil
			}
			for _, p := range problems {
				fmt.Fprintf(a.IO.Err, "%s %s\n", a.IO.ErrTheme.Danger.Render("✗"), p)
			}
			return &UserError{Message: fmt.Sprintf("%s has %d problem(s).", file, len(problems))}
		},
	})
	return cmd
}

// validateConfig parses a teral.toml and checks it against the schema. It
// returns readable problems, or an error when the file can't be read.
func validateConfig(file string) ([]string, error) {
	raw, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, &UserError{Message: file + " doesn't exist.", Hint: "Create one with `teral config init`."}
	}
	if err != nil {
		return nil, err
	}

	var doc map[string]any
	if _, err := toml.Decode(string(raw), &doc); err != nil {
		var perr toml.ParseError
		if errors.As(err, &perr) {
			return []string{fmt.Sprintf("line %d: %s", perr.Position.Line, perr.Message)}, nil
		}
		return []string{err.Error()}, nil
	}

	// Round-trip through JSON so numbers and nested tables have the types
	// the validator expects.
	encoded, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}

	compiler := jsonschema.NewCompiler()
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema.JSON))
	if err != nil {
		return nil, fmt.Errorf("embedded schema: %w", err)
	}
	if err := compiler.AddResource(schema.URL, schemaDoc); err != nil {
		return nil, err
	}
	sch, err := compiler.Compile(schema.URL)
	if err != nil {
		return nil, fmt.Errorf("embedded schema: %w", err)
	}

	err = sch.Validate(instance)
	if err == nil {
		return nil, nil
	}
	var verr *jsonschema.ValidationError
	if !errors.As(err, &verr) {
		return nil, err
	}
	var problems []string
	collectProblems(verr.DetailedOutput(), &problems)
	if len(problems) == 0 {
		problems = []string{verr.Error()}
	}
	return problems, nil
}

func collectProblems(unit *jsonschema.OutputUnit, out *[]string) {
	if unit == nil {
		return
	}
	if unit.Error != nil && len(unit.Errors) == 0 {
		loc := strings.TrimPrefix(strings.ReplaceAll(unit.InstanceLocation, "/", "."), ".")
		if loc == "" {
			loc = "(top level)"
		}
		*out = append(*out, fmt.Sprintf("%s: %s", loc, unit.Error.String()))
	}
	for i := range unit.Errors {
		collectProblems(&unit.Errors[i], out)
	}
}
