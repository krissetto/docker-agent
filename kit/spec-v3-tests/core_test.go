package specv3_test

import (
	"os"
	"strings"
	"testing"

	"github.com/docker/sandbox-kit-spec/v3/resolve"
	"github.com/docker/sandbox-kit-spec/v3/spec"
	"github.com/stretchr/testify/require"
)

func parse(t *testing.T, raw string) *spec.Descriptor {
	t.Helper()
	d, err := spec.Decode([]byte(raw))
	require.NoError(t, err)
	_, err = spec.ValidateRaw([]byte(raw), d)
	require.NoError(t, err)
	return d
}

func fixture(t *testing.T, name string) (string, *spec.Descriptor) {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	return string(raw), parse(t, string(raw))
}

func document(body string) string { return "schemaVersion: \"3\"\nkind: workload\n" + body + "\n" }
func capDocument(typ, config string) string {
	body := "capabilities:\n  - type: com.docker.sandbox/" + typ + "\n"
	if config != "" {
		body += "    config: " + config + "\n"
	}
	return document(body)
}
func validationError(raw string) error {
	d, err := spec.Decode([]byte(raw))
	if err != nil {
		return err
	}
	_, err = spec.ValidateRaw([]byte(raw), d)
	return err
}

func TestAuthoringValidation(t *testing.T) {
	for _, name := range []string{"workload.yaml", "mixin.yaml", "companion.yaml", "set.yaml", "args.yaml"} {
		t.Run(name, func(t *testing.T) { fixture(t, name) })
	}
	cases := map[string]string{
		"unknown top level":          "invented: true",
		"obsolete relation name":     "integratesWith: [peer]",
		"invalid icon":               "iconUrl: http://example.invalid/icon.png",
		"build dockerfile exclusive": "build: FROM scratch\ndockerfile: recipe",
		"build kits exclusive":       "build: FROM scratch\nkits: [{ref: 'example.invalid/kit:1'}]",
		"dockerfile kits exclusive":  "dockerfile: recipe\nkits: [{ref: 'example.invalid/kit:1'}]",
		"recipe escapes":             "dockerfile: ../outside",
		"recipe absolute":            "dockerfile: /outside",
		"set local reference":        "kits: [{ref: ./local}]",
		"set malformed digest":       "kits: [{ref: 'example.invalid/kit:1', digest: 'sha256:no'}]",
		"set duplicate ref":          "kits: [{ref: 'example.invalid/kit:1'}, {ref: 'example.invalid/kit:1'}]",
		"arg path name":              "args: {'../path': {default: x}}",
		"arg default required":       "args: {x: {default: x, required: true}}",
		"arg enum pattern":           "args: {x: {enum: [x], pattern: x}}",
		"arg env build":              "args: {x: {env: X, buildArg: X}}",
		"arg bad env":                "args: {x: {env: 'BAD-NAME'}}",
		"arg bad build name":         "args: {x: {buildArg: '../BAD'}}",
		"arg bad regex":              "args: {x: {pattern: '['}}",
		"arg unknown field":          "args: {x: {defalt: x}}",
		"undeclared reference":       "description: '${{ kit.args.missing }}'",
		"requires reference":         "args: {x: {default: peer}}\nrequires: ['${{ kit.args.x }}']",
		"impossible constraint":      "requires: ['peer >= 2, < 1']",
		"bad namespace":              "provides: [single/peer@1]",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) { require.Error(t, validationError(document(body))) })
	}
	t.Run("empty set", func(t *testing.T) { require.Error(t, validationError("schemaVersion: \"3\"\nkind: set\n")) })
	t.Run("reserved package namespace", func(t *testing.T) {
		d := parse(t, document("provides: [deb/bash@5.2]"))
		require.Error(t, spec.RequireAuthoredProvides(d))
	})
	t.Run("source positions", func(t *testing.T) {
		raw := document("dockerfile: ../outside")
		err := validationError(raw)
		var field *spec.FieldError
		require.ErrorAs(t, err, &field)
		require.Contains(t, spec.Positions([]byte(raw)), "dockerfile")
	})
}

func TestArgPhases(t *testing.T) {
	raw, d := fixture(t, "args.yaml")
	values, err := spec.ResolveArgs(d.Args, map[string]string{"team": "beta", "text": "quotes \" and \\ slash\nnewline"})
	require.NoError(t, err)
	require.Equal(t, "8080", values["port"])
	require.Equal(t, "beta", values["team"])
	built, err := spec.ExpandBuildArgs([]byte(raw), d.Args, values)
	require.NoError(t, err)
	published := parse(t, string(built))
	require.Equal(t, []string{"fixture@0.1.0"}, published.Provides)
	_, err = spec.ValidatePublished(built, published)
	require.NoError(t, err)
	require.Contains(t, string(built), "${{ kit.args.port }}")
	effective, err := spec.ExpandCreateArgs(built, published.Args, values)
	require.NoError(t, err)
	ed := parse(t, string(effective))
	_, err = spec.ValidateEffective(effective, ed)
	require.NoError(t, err)
	ports, err := spec.PortsOf(ed.Capabilities)
	require.NoError(t, err)
	require.Equal(t, 8080, ports[0].Container)
	lc, err := spec.LifecycleOf(ed.Capabilities)
	require.NoError(t, err)
	require.Equal(t, "prefix:"+values["text"]+":$HOME:${HOME}", lc.Files[0].Content)
	require.Contains(t, raw, "${{ kit.args.version }}")
	for name, supplied := range map[string]map[string]string{
		"missing required": {}, "unknown input": {"team": "alpha", "typo": "x"},
		"enum": {"team": "gamma"}, "whole pattern": {"team": "alpha", "port": "x8080"},
	} {
		t.Run(name, func(t *testing.T) { _, err := spec.ResolveArgs(d.Args, supplied); require.Error(t, err) })
	}
	t.Run("strict effective port", func(t *testing.T) {
		badValues, err := spec.ResolveArgs(d.Args, map[string]string{"team": "alpha", "port": "70000"})
		require.NoError(t, err)
		out, err := spec.ExpandCreateArgs(built, published.Args, badValues)
		require.NoError(t, err)
		bad, err := spec.Decode(out)
		require.NoError(t, err)
		_, err = spec.ValidateEffective(out, bad)
		require.Error(t, err)
	})
	t.Run("map key collision", func(t *testing.T) {
		raw := document("args: {x: {default: fixed}}\ncapabilities:\n- type: org.example/extension@1\n  config: {'${{ kit.args.x }}': a, fixed: b}")
		d := parse(t, raw)
		_, err := spec.ExpandCreateArgs([]byte(raw), d.Args, map[string]string{"x": "fixed"})
		require.Error(t, err)
	})
	for _, value := range []string{"007", "1.0", "1e5", "NaN"} {
		t.Run("roundtrip "+value, func(t *testing.T) {
			raw := document("args: {x: {}}\ncapabilities:\n- type: org.example/extension@1\n  config: {value: '${{ kit.args.x }}'}")
			d := parse(t, raw)
			out, err := spec.ExpandCreateArgs([]byte(raw), d.Args, map[string]string{"x": value})
			require.NoError(t, err)
			require.Equal(t, value, parse(t, string(out)).Capabilities[0].Config["value"])
		})
	}
}

func TestPublishedValidation(t *testing.T) {
	for name, raw := range map[string]string{
		"unversioned provide": document("provides: [peer]"),
		"unexpanded build":    document("args: {version: {default: '1', buildArg: VERSION}}\nprovides: ['peer@${{ kit.args.version }}']"),
		"authoring set kind":  "schemaVersion: \"3\"\nkind: set\nkits: [{ref: 'example.invalid/kit:1'}]\n",
		"missing kit digest":  document("kits: [{ref: 'example.invalid/kit:1'}]"),
	} {
		t.Run(name, func(t *testing.T) {
			d := parse(t, raw)
			_, err := spec.ValidatePublished([]byte(raw), d)
			require.Error(t, err)
		})
	}
	t.Run("size budgets", func(t *testing.T) {
		for _, size := range []int{spec.SizeWarnBytes + 1, spec.SizeErrorBytes + 1} {
			raw := document("description: " + strings.Repeat("x", size))
			d, err := spec.Decode([]byte(raw))
			require.NoError(t, err)
			warnings, err := spec.ValidateRaw([]byte(raw), d)
			if size > spec.SizeErrorBytes {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.NotEmpty(t, warnings)
			}
		}
	})
}

func TestPublishedWorkload(t *testing.T) {
	raw, err := os.ReadFile("../async-agent.yaml")
	require.NoError(t, err)
	d := parse(t, string(raw))
	require.Equal(t, spec.KindWorkload, d.Kind)
	buildDecls := map[string]spec.Arg{}
	for name, arg := range d.Args {
		if arg.BuildArg != "" {
			buildDecls[name] = arg
		}
	}
	values, err := spec.ResolveArgs(buildDecls, nil)
	require.NoError(t, err)
	published, err := spec.ExpandBuildArgs(raw, d.Args, values)
	require.NoError(t, err)
	pd := parse(t, string(published))
	_, err = spec.ValidatePublished(published, pd)
	require.NoError(t, err)
	require.NoError(t, spec.RequireAuthoredProvides(d))
	require.True(t, spec.HasCapability(pd.Capabilities, spec.CapabilitySbx))
	require.False(t, spec.HasCapability(pd.Capabilities, spec.CapabilityAgentContext))
	_, mixin := fixture(t, "mixin.yaml")
	_, err = resolve.Resolve([]*resolve.Unit{{Reference: "async-agent-kit", Descriptor: pd}, {Reference: "fixture-mixin", Descriptor: mixin}})
	require.NoError(t, err)
	require.Equal(t, pd.DisplayName, spec.OCIAnnotations(pd)["org.opencontainers.image.title"])
	createDecls := map[string]spec.Arg{}
	for name, arg := range pd.Args {
		if arg.BuildArg == "" {
			createDecls[name] = arg
		}
	}
	createValues, err := spec.ResolveArgs(createDecls, nil)
	require.NoError(t, err)
	effective, err := spec.ExpandCreateArgs(published, pd.Args, createValues)
	require.NoError(t, err)
	ed := parse(t, string(effective))
	_, err = spec.ValidateEffective(effective, ed)
	require.NoError(t, err)
}
