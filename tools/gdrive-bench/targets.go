//go:build !no_extra_providers

package main

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/repo/blob"
)

// target is one backend the suite runs against.
type target struct {
	// Name is the label the result document and every comparison uses.
	Name string

	// Conn is the decoded connection info handed to blob.NewStorage.
	Conn blob.ConnectionInfo

	// Knobs is the "tuning" object of the connection config exactly as the
	// targets file provided it (nil when the file did not set one, meaning the
	// backend's built-in defaults are in force).
	Knobs map[string]any

	// RedactedConfig is the connection config with every credential-bearing key
	// replaced, safe to embed in a result document.
	RedactedConfig map[string]any
}

// rawTarget is the on-disk shape of one entry of the targets file.
type rawTarget struct {
	Name       string          `json:"name"`
	Connection json.RawMessage `json:"connection"`
}

// varPattern matches the ${VAR} placeholders expanded in a targets file. Only
// the braced form is recognized, so a bare "$" never has to be escaped.
var varPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// sensitiveKeySubstrings are the (lowercased) substrings that mark a JSON key
// as credential-bearing. A key matching any of them is replaced by a redaction
// marker before its value can reach a result document. The list is deliberately
// broad: over-redacting a knob costs a line in a report, under-redacting one
// leaks a refresh token into a file that gets published.
var sensitiveKeySubstrings = []string{"credential", "secret", "token", "password", "passphrase", "key"}

// redactedMarker replaces the value of a sensitive key.
const redactedMarker = "[redacted]"

// expandVars replaces every ${VAR} in s using lookup. An undefined variable is
// an error: silently expanding it to "" would produce a target that connects
// somewhere unintended.
func expandVars(s string, lookup func(string) (string, bool)) (string, error) {
	var missing []string

	out := varPattern.ReplaceAllStringFunc(s, func(m string) string {
		name := varPattern.FindStringSubmatch(m)[1]

		v, ok := lookup(name)
		if !ok {
			missing = append(missing, name)

			return m
		}

		return v
	})

	if len(missing) > 0 {
		return "", errors.Errorf("undefined environment variable(s): %v", strings.Join(dedupe(missing), ", "))
	}

	return out, nil
}

// dedupe returns the sorted, de-duplicated contents of in.
func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := []string{}

	for _, v := range in {
		if !seen[v] {
			seen[v] = true

			out = append(out, v)
		}
	}

	sort.Strings(out)

	return out
}

// expandTree walks a decoded JSON document and expands ${VAR} in every string
// it contains, including map keys. Expansion happens after parsing so a value
// holding a quote or a backslash can never corrupt the document.
func expandTree(v any, lookup func(string) (string, bool)) (any, error) {
	switch t := v.(type) {
	case string:
		return expandVars(t, lookup)

	case []any:
		out := make([]any, len(t))

		for i, e := range t {
			ev, err := expandTree(e, lookup)
			if err != nil {
				return nil, err
			}

			out[i] = ev
		}

		return out, nil

	case map[string]any:
		out := make(map[string]any, len(t))

		for k, e := range t {
			ek, err := expandVars(k, lookup)
			if err != nil {
				return nil, err
			}

			ev, err := expandTree(e, lookup)
			if err != nil {
				return nil, err
			}

			out[ek] = ev
		}

		return out, nil

	default:
		return v, nil
	}
}

// redactTree returns a deep copy of a decoded JSON document with the value of
// every credential-bearing key replaced by redactedMarker.
func redactTree(v any) any {
	switch t := v.(type) {
	case []any:
		out := make([]any, len(t))

		for i, e := range t {
			out[i] = redactTree(e)
		}

		return out

	case map[string]any:
		out := make(map[string]any, len(t))

		for k, e := range t {
			if isSensitiveKey(k) {
				out[k] = redactedMarker

				continue
			}

			out[k] = redactTree(e)
		}

		return out

	default:
		return v
	}
}

// isSensitiveKey reports whether a JSON key is credential-bearing.
func isSensitiveKey(k string) bool {
	lk := strings.ToLower(k)

	for _, s := range sensitiveKeySubstrings {
		if strings.Contains(lk, s) {
			return true
		}
	}

	return false
}

// loadTargets reads and decodes a targets file. The file is either a JSON array
// of targets or an object with a "targets" array.
func loadTargets(path string, lookup func(string) (string, bool)) ([]target, error) {
	//nolint:gosec // the path is the operator's own configuration file, named on the command line.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to read the targets file %v", path)
	}

	return parseTargets(data, lookup)
}

// parseTargets decodes the contents of a targets file.
func parseTargets(data []byte, lookup func(string) (string, bool)) ([]target, error) {
	raws, err := decodeRawTargets(data)
	if err != nil {
		return nil, err
	}

	if len(raws) == 0 {
		return nil, errors.New("the targets file defines no targets")
	}

	res := make([]target, 0, len(raws))
	seen := map[string]bool{}

	for i, r := range raws {
		t, err := buildTarget(r, lookup)
		if err != nil {
			return nil, errors.Wrapf(err, "target #%v", i)
		}

		if seen[t.Name] {
			return nil, errors.Errorf("duplicate target name %q", t.Name)
		}

		seen[t.Name] = true

		res = append(res, t)
	}

	return res, nil
}

// decodeRawTargets accepts both accepted shapes of the targets file.
func decodeRawTargets(data []byte) ([]rawTarget, error) {
	var arr []rawTarget

	if err := json.Unmarshal(data, &arr); err == nil {
		return arr, nil
	}

	wrapped := struct {
		Targets []rawTarget `json:"targets"`
	}{}

	if err := json.Unmarshal(data, &wrapped); err != nil {
		return nil, errors.Wrap(err, "the targets file must be a JSON array of targets or an object with a \"targets\" array")
	}

	return wrapped.Targets, nil
}

// buildTarget expands, redacts and decodes one raw target.
func buildTarget(r rawTarget, lookup func(string) (string, bool)) (target, error) {
	name, err := expandVars(r.Name, lookup)
	if err != nil {
		return target{}, err
	}

	if name == "" {
		return target{}, errors.New("every target needs a non-empty \"name\"")
	}

	if len(r.Connection) == 0 {
		return target{}, errors.Errorf("target %q has no \"connection\"", name)
	}

	var tree any

	if err := json.Unmarshal(r.Connection, &tree); err != nil {
		return target{}, errors.Wrapf(err, "target %q has an invalid \"connection\"", name)
	}

	expanded, err := expandTree(tree, lookup)
	if err != nil {
		return target{}, errors.Wrapf(err, "target %q", name)
	}

	expandedJSON, err := json.Marshal(expanded)
	if err != nil {
		return target{}, errors.Wrapf(err, "target %q: unable to re-encode the connection", name)
	}

	var ci blob.ConnectionInfo

	// ConnectionInfo.UnmarshalJSON resolves the storage type through blob's
	// factory registry, so an unregistered provider is reported here.
	if err := json.Unmarshal(expandedJSON, &ci); err != nil {
		return target{}, errors.Wrapf(err, "target %q has an unusable \"connection\"", name)
	}

	knobs, redacted := configViews(expanded)

	return target{Name: name, Conn: ci, Knobs: knobs, RedactedConfig: redacted}, nil
}

// configViews extracts the tuning knobs and the redacted config from an
// expanded connection document.
func configViews(expanded any) (knobs, redacted map[string]any) {
	m, ok := expanded.(map[string]any)
	if !ok {
		return nil, nil
	}

	cfg, ok := m["config"].(map[string]any)
	if !ok {
		return nil, nil
	}

	if t, ok := cfg["tuning"].(map[string]any); ok {
		//nolint:forcetypeassert // redactTree of a map is always a map.
		knobs = redactTree(t).(map[string]any)
	}

	//nolint:forcetypeassert // redactTree of a map is always a map.
	redacted = redactTree(cfg).(map[string]any)

	return knobs, redacted
}

// openStorage connects to a target. isCreate is passed through to the provider,
// which lets the filesystem backend create the directory of a temp-dir target.
func (t target) openStorage(ctx context.Context) (blob.Storage, error) {
	st, err := blob.NewStorage(ctx, t.Conn, true)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to connect to target %q", t.Name)
	}

	return st, nil
}

// envLookup is the process environment, in the shape expandVars wants.
func envLookup(name string) (string, bool) {
	return os.LookupEnv(name)
}
