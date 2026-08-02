//go:build !no_extra_providers

package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func lookupMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]

		return v, ok
	}
}

func TestExpandVars(t *testing.T) {
	lookup := lookupMap(map[string]string{"A": "1", "B_2": "two", "EMPTY": ""})

	cases := []struct{ in, want string }{
		{"", ""},
		{"nothing", "nothing"},
		{"${A}", "1"},
		{"x${A}y${B_2}z", "x1ytwoz"},
		{"${EMPTY}", ""},
		{"$A", "$A"},
		{"${A}${A}", "11"},
	}

	for _, tc := range cases {
		got, err := expandVars(tc.in, lookup)
		require.NoError(t, err)
		require.Equal(t, tc.want, got, "input %q", tc.in)
	}
}

func TestExpandVarsUndefinedIsAnError(t *testing.T) {
	_, err := expandVars("${MISSING}/${ALSO_MISSING}/${MISSING}", lookupMap(nil))
	require.Error(t, err)
	require.Contains(t, err.Error(), "ALSO_MISSING")
	require.Contains(t, err.Error(), "MISSING")
}

func TestExpandedValuesCannotCorruptTheDocument(t *testing.T) {
	// Expansion happens after parsing, so a value full of JSON metacharacters
	// lands in the config verbatim instead of breaking out of its string.
	nasty := `","type":"evil","x":"`
	doc := []byte(`[{"name":"t","connection":{"type":"filesystem","config":{"path":"${NASTY}"}}}]`)

	got, err := parseTargets(doc, lookupMap(map[string]string{"NASTY": nasty}))
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "filesystem", got[0].Conn.Type)
	require.Equal(t, nasty, got[0].RedactedConfig["path"])
}

func TestParseTargetsExpandsAndDecodes(t *testing.T) {
	doc := []byte(`[
	  {"name":"native-${SUFFIX}","connection":{"type":"gdrive","config":{
	     "folderID":"${FOLDER}",
	     "credentialsFile":"${CREDS}",
	     "tuning":{"pacerMinSleepMS":50,"listPageSize":500}}}},
	  {"name":"fs","connection":{"type":"filesystem","config":{"path":"/tmp/x"}}}
	]`)

	got, err := parseTargets(doc, lookupMap(map[string]string{
		"SUFFIX": "a",
		"FOLDER": "folder-123",
		"CREDS":  "/home/u/creds.json",
	}))
	require.NoError(t, err)
	require.Len(t, got, 2)

	require.Equal(t, "native-a", got[0].Name)
	require.Equal(t, "gdrive", got[0].Conn.Type)
	require.Equal(t, map[string]any{"pacerMinSleepMS": 50.0, "listPageSize": 500.0}, got[0].Knobs)
	require.Equal(t, "folder-123", got[0].RedactedConfig["folderID"])
	require.Equal(t, redactedMarker, got[0].RedactedConfig["credentialsFile"], "credential paths must never reach a result document")

	require.Equal(t, "fs", got[1].Name)
	require.Nil(t, got[1].Knobs)
}

func TestParseTargetsAcceptsWrappedForm(t *testing.T) {
	doc := []byte(`{"targets":[{"name":"fs","connection":{"type":"filesystem","config":{"path":"/tmp/x"}}}]}`)

	got, err := parseTargets(doc, lookupMap(nil))
	require.NoError(t, err)
	require.Len(t, got, 1)
}

func TestParseTargetsRejections(t *testing.T) {
	cases := map[string]string{
		"empty array":       `[]`,
		"missing name":      `[{"connection":{"type":"filesystem","config":{"path":"/x"}}}]`,
		"missing conn":      `[{"name":"a"}]`,
		"unknown type":      `[{"name":"a","connection":{"type":"nosuchprovider","config":{}}}]`,
		"duplicate names":   `[{"name":"a","connection":{"type":"filesystem","config":{"path":"/x"}}},{"name":"a","connection":{"type":"filesystem","config":{"path":"/y"}}}]`,
		"not even json":     `not json`,
		"undefined var ref": `[{"name":"a","connection":{"type":"filesystem","config":{"path":"${NOPE}"}}}]`,
	}

	for name, doc := range cases {
		_, err := parseTargets([]byte(doc), lookupMap(nil))
		require.Error(t, err, name)
	}
}

func TestRedactTree(t *testing.T) {
	in := map[string]any{
		"folderID":        "public",
		"credentials":     map[string]any{"refresh_token": "s3cret"},
		"clientSecret":    "s3cret",
		"tokenCacheFile":  "/home/u/token.json",
		"apiKey":          "s3cret",
		"nested":          []any{map[string]any{"password": "s3cret", "ok": 1.0}},
		"UPPER_PASSWORD":  "s3cret",
		"harmlessSetting": 5.0,
	}

	out, ok := redactTree(in).(map[string]any)
	require.True(t, ok)

	require.Equal(t, "public", out["folderID"])
	require.Equal(t, redactedMarker, out["credentials"])
	require.Equal(t, redactedMarker, out["clientSecret"])
	require.Equal(t, redactedMarker, out["tokenCacheFile"])
	require.Equal(t, redactedMarker, out["apiKey"])
	require.Equal(t, redactedMarker, out["UPPER_PASSWORD"])
	require.InDelta(t, 5.0, out["harmlessSetting"], 1e-9)

	nested, ok := out["nested"].([]any)
	require.True(t, ok)

	first, ok := nested[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, redactedMarker, first["password"])
	require.InDelta(t, 1.0, first["ok"], 1e-9)

	// The input is not modified.
	require.Equal(t, "s3cret", in["clientSecret"])
}

func TestLoadTargetsFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "targets.json")

	require.NoError(t, writeFileAtomic(path, []byte(`[{"name":"fs","connection":{"type":"filesystem","config":{"path":"`+dir+`"}}}]`)))

	got, err := loadTargets(path, lookupMap(nil))
	require.NoError(t, err)
	require.Len(t, got, 1)

	_, err = loadTargets(filepath.Join(dir, "nope.json"), lookupMap(nil))
	require.Error(t, err)
}
