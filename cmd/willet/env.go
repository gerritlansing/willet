package main

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/joho/godotenv"
	"github.com/spf13/pflag"
)

const (
	envPrefix  = "WILLET_"
	envFileVar = envPrefix + "ENV_FILE"
)

// envName returns the environment variable that backs a flag.
func envName(flag string) string {
	return envPrefix + strings.ToUpper(strings.ReplaceAll(flag, "-", "_"))
}

// readEnvFile parses an env file. Unknown WILLET_* keys are rejected so
// a typo fails loudly instead of silently falling back to a default; other
// keys are ignored, so the file can be shared with other tools.
func readEnvFile(path string, fs *pflag.FlagSet) (map[string]string, error) {
	vars, err := godotenv.Read(path)
	if err != nil {
		return nil, fmt.Errorf("read env file %s: %w", path, err)
	}

	known := map[string]bool{}
	fs.VisitAll(func(f *pflag.Flag) { known[envName(f.Name)] = true })

	var unknown []string
	for k := range vars {
		if strings.HasPrefix(k, envPrefix) && !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return nil, fmt.Errorf("env file %s: unknown settings: %s", path, strings.Join(unknown, ", "))
	}
	return vars, nil
}

// applyEnv fills flags not set on the command line. Precedence, highest
// first: command-line flag, process environment, env file.
func applyEnv(fs *pflag.FlagSet, fileVars map[string]string) error {
	var errs []error
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Changed {
			return
		}
		name := envName(f.Name)
		v, ok := os.LookupEnv(name)
		if !ok {
			v, ok = fileVars[name]
		}
		if ok {
			if err := fs.Set(f.Name, v); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
			}
		}
	})
	return errors.Join(errs...)
}

// loadEnv applies the process environment and the env file (the --env-file
// value, falling back to WILLET_ENV_FILE) to flags.
func loadEnv(fs *pflag.FlagSet, envFile string) error {
	if envFile == "" {
		envFile = os.Getenv(envFileVar)
	}
	var fileVars map[string]string
	if envFile != "" {
		var err error
		if fileVars, err = readEnvFile(envFile, fs); err != nil {
			return err
		}
		if _, ok := fileVars[envFileVar]; ok {
			return fmt.Errorf("env file %s: %s cannot be set inside an env file", envFile, envFileVar)
		}
	}
	return applyEnv(fs, fileVars)
}
