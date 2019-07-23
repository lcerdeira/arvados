// Copyright (C) The Arvados Authors. All rights reserved.
//
// SPDX-License-Identifier: AGPL-3.0

package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"strings"

	"git.curoverse.com/arvados.git/sdk/go/arvados"
	"github.com/ghodss/yaml"
	"github.com/imdario/mergo"
	"github.com/sirupsen/logrus"
)

var ErrNoClustersDefined = errors.New("config does not define any clusters")

type Loader struct {
	Stdin          io.Reader
	Logger         logrus.FieldLogger
	SkipDeprecated bool // Don't load legacy/deprecated config keys/files

	Path                    string
	KeepstorePath           string
	CrunchDispatchSlurmPath string
	WebsocketPath           string

	configdata []byte
}

// NewLoader returns a new Loader with Stdin and Logger set to the
// given values, and all config paths set to their default values.
func NewLoader(stdin io.Reader, logger logrus.FieldLogger) *Loader {
	ldr := &Loader{Stdin: stdin, Logger: logger}
	// Calling SetupFlags on a throwaway FlagSet has the side
	// effect of assigning default values to the configurable
	// fields.
	ldr.SetupFlags(flag.NewFlagSet("", flag.ContinueOnError))
	return ldr
}

// SetupFlags configures a flagset so arguments like -config X can be
// used to change the loader's Path fields.
//
//	ldr := NewLoader(os.Stdin, logrus.New())
//	flagset := flag.NewFlagSet("", flag.ContinueOnError)
//	ldr.SetupFlags(flagset)
//	// ldr.Path == "/etc/arvados/config.yml"
//	flagset.Parse([]string{"-config", "/tmp/c.yaml"})
//	// ldr.Path == "/tmp/c.yaml"
func (ldr *Loader) SetupFlags(flagset *flag.FlagSet) {
	flagset.StringVar(&ldr.Path, "config", arvados.DefaultConfigFile, "Site configuration `file` (default may be overridden by setting an ARVADOS_CONFIG environment variable)")
	flagset.StringVar(&ldr.KeepstorePath, "legacy-keepstore-config", defaultKeepstoreConfigPath, "Legacy keepstore configuration `file`")
	flagset.StringVar(&ldr.CrunchDispatchSlurmPath, "legacy-crunch-dispatch-slurm-config", defaultCrunchDispatchSlurmConfigPath, "Legacy crunch-dispatch-slurm configuration `file`")
	flagset.StringVar(&ldr.WebsocketPath, "legacy-ws-config", defaultWebsocketConfigPath, "Legacy arvados-ws configuration `file`")
}

// MungeLegacyConfigArgs checks args for a -config flag whose argument
// is a regular file (or a symlink to one), but doesn't have a
// top-level "Clusters" key and therefore isn't a valid cluster
// configuration file. If it finds such a flag, it replaces -config
// with legacyConfigArg (e.g., "-legacy-keepstore-config").
//
// This is used by programs that still need to accept "-config" as a
// way to specify a per-component config file until their config has
// been migrated.
//
// If any errors are encountered while reading or parsing a config
// file, the given args are not munged. We presume the same errors
// will be encountered again and reported later on when trying to load
// cluster configuration from the same file, regardless of which
// struct we end up using.
func (ldr *Loader) MungeLegacyConfigArgs(lgr logrus.FieldLogger, args []string, legacyConfigArg string) []string {
	munged := append([]string(nil), args...)
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") || strings.SplitN(strings.TrimPrefix(args[i], "-"), "=", 2)[0] != "config" {
			continue
		}
		var operand string
		if strings.Contains(args[i], "=") {
			operand = strings.SplitN(args[i], "=", 2)[1]
		} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			i++
			operand = args[i]
		} else {
			continue
		}
		if fi, err := os.Stat(operand); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		f, err := os.Open(operand)
		if err != nil {
			continue
		}
		defer f.Close()
		buf, err := ioutil.ReadAll(f)
		if err != nil {
			continue
		}
		var cfg arvados.Config
		err = yaml.Unmarshal(buf, &cfg)
		if err != nil {
			continue
		}
		if len(cfg.Clusters) == 0 {
			lgr.Warnf("%s is not a cluster config file -- interpreting %s as %s (please migrate your config!)", operand, "-config", legacyConfigArg)
			if operand == args[i] {
				munged[i-1] = legacyConfigArg
			} else {
				munged[i] = legacyConfigArg + "=" + operand
			}
		}
	}
	return munged
}

func (ldr *Loader) loadBytes(path string) ([]byte, error) {
	if path == "-" {
		return ioutil.ReadAll(ldr.Stdin)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ioutil.ReadAll(f)
}

func (ldr *Loader) Load() (*arvados.Config, error) {
	if ldr.configdata == nil {
		buf, err := ldr.loadBytes(ldr.Path)
		if err != nil {
			return nil, err
		}
		ldr.configdata = buf
	}

	// Load the config into a dummy map to get the cluster ID
	// keys, discarding the values; then set up defaults for each
	// cluster ID; then load the real config on top of the
	// defaults.
	var dummy struct {
		Clusters map[string]struct{}
	}
	err := yaml.Unmarshal(ldr.configdata, &dummy)
	if err != nil {
		return nil, err
	}
	if len(dummy.Clusters) == 0 {
		return nil, ErrNoClustersDefined
	}

	// We can't merge deep structs here; instead, we unmarshal the
	// default & loaded config files into generic maps, merge
	// those, and then json-encode+decode the result into the
	// config struct type.
	var merged map[string]interface{}
	for id := range dummy.Clusters {
		var src map[string]interface{}
		err = yaml.Unmarshal(bytes.Replace(DefaultYAML, []byte(" xxxxx:"), []byte(" "+id+":"), -1), &src)
		if err != nil {
			return nil, fmt.Errorf("loading defaults for %s: %s", id, err)
		}
		err = mergo.Merge(&merged, src, mergo.WithOverride)
		if err != nil {
			return nil, fmt.Errorf("merging defaults for %s: %s", id, err)
		}
	}
	var src map[string]interface{}
	err = yaml.Unmarshal(ldr.configdata, &src)
	if err != nil {
		return nil, fmt.Errorf("loading config data: %s", err)
	}
	ldr.logExtraKeys(merged, src, "")
	removeSampleKeys(merged)
	err = mergo.Merge(&merged, src, mergo.WithOverride)
	if err != nil {
		return nil, fmt.Errorf("merging config data: %s", err)
	}

	// map[string]interface{} => json => arvados.Config
	var cfg arvados.Config
	var errEnc error
	pr, pw := io.Pipe()
	go func() {
		errEnc = json.NewEncoder(pw).Encode(merged)
		pw.Close()
	}()
	err = json.NewDecoder(pr).Decode(&cfg)
	if errEnc != nil {
		err = errEnc
	}
	if err != nil {
		return nil, fmt.Errorf("transcoding config data: %s", err)
	}

	if !ldr.SkipDeprecated {
		err = ldr.applyDeprecatedConfig(&cfg)
		if err != nil {
			return nil, err
		}
		// legacy file is required when either:
		// * a non-default location was specified
		// * no primary config was loaded, and this is the
		// legacy config file for the current component
		for _, err := range []error{
			ldr.loadOldKeepstoreConfig(&cfg),
			ldr.loadOldCrunchDispatchSlurmConfig(&cfg),
			ldr.loadOldWebsocketConfig(&cfg),
		} {
			if err != nil {
				return nil, err
			}
		}
	}

	// Check for known mistakes
	for id, cc := range cfg.Clusters {
		err = checkKeyConflict(fmt.Sprintf("Clusters.%s.PostgreSQL.Connection", id), cc.PostgreSQL.Connection)
		if err != nil {
			return nil, err
		}
	}
	return &cfg, nil
}

func checkKeyConflict(label string, m map[string]string) error {
	saw := map[string]bool{}
	for k := range m {
		k = strings.ToLower(k)
		if saw[k] {
			return fmt.Errorf("%s: multiple entries for %q (fix by using same capitalization as default/example file)", label, k)
		}
		saw[k] = true
	}
	return nil
}

func removeSampleKeys(m map[string]interface{}) {
	delete(m, "SAMPLE")
	for _, v := range m {
		if v, _ := v.(map[string]interface{}); v != nil {
			removeSampleKeys(v)
		}
	}
}

func (ldr *Loader) logExtraKeys(expected, supplied map[string]interface{}, prefix string) {
	if ldr.Logger == nil {
		return
	}
	allowed := map[string]interface{}{}
	for k, v := range expected {
		allowed[strings.ToLower(k)] = v
	}
	for k, vsupp := range supplied {
		if k == "SAMPLE" {
			// entry will be dropped in removeSampleKeys anyway
			continue
		}
		vexp, ok := allowed[strings.ToLower(k)]
		if expected["SAMPLE"] != nil {
			vexp = expected["SAMPLE"]
		} else if !ok {
			ldr.Logger.Warnf("deprecated or unknown config entry: %s%s", prefix, k)
			continue
		}
		if vsupp, ok := vsupp.(map[string]interface{}); !ok {
			// if vsupp is a map but vexp isn't map, this
			// will be caught elsewhere; see TestBadType.
			continue
		} else if vexp, ok := vexp.(map[string]interface{}); !ok {
			ldr.Logger.Warnf("unexpected object in config entry: %s%s", prefix, k)
		} else {
			ldr.logExtraKeys(vexp, vsupp, prefix+k+".")
		}
	}
}
