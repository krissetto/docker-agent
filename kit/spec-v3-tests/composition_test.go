package specv3_test

import (
	"strings"
	"testing"

	"github.com/docker/sandbox-kit-spec/v3/resolve"
	"github.com/docker/sandbox-kit-spec/v3/spec"
	"github.com/stretchr/testify/require"
)

func unit(t *testing.T, ref, kind, body string) *resolve.Unit {
	t.Helper()
	return &resolve.Unit{Reference: ref, Digest: "sha256:" + strings.Repeat("a", 64), Descriptor: parse(t, "schemaVersion: \"3\"\nkind: "+kind+"\n"+body+"\n")}
}
func TestResolution(t *testing.T) {
	_, wd := fixture(t, "workload.yaml")
	_, md := fixture(t, "mixin.yaml")
	w := &resolve.Unit{Reference: "z-workload", Descriptor: wd}
	m := &resolve.Unit{Reference: "a-mixin", Descriptor: md}
	r, err := resolve.Resolve([]*resolve.Unit{m, w})
	require.NoError(t, err)
	require.Equal(t, []*resolve.Unit{w, m}, r.Topological())
	require.Equal(t, []*resolve.Unit{w, m}, r.Ordered())
	tests := map[string][]*resolve.Unit{
		"missing provider":              {m},
		"self requirement":              {unit(t, "self", "workload", "provides: [self@1]\nrequires: [self]")},
		"duplicate provider normalized": {unit(t, "w", "workload", "provides: [peer@1]"), unit(t, "m", "mixin", "provides: [com.docker.kit/peer@2]")},
		"conflict":                      {w, unit(t, "m", "mixin", "conflicts: [async-agent-kit]")},
		"present integration fails":     {w, unit(t, "m", "mixin", "integrates: ['async-agent-kit >= 2']")},
		"cycle":                         {unit(t, "w", "workload", "provides: [a@1]\nrequires: [b]"), unit(t, "m", "mixin", "provides: [b@1]\nrequires: [a]")},
		"two workloads":                 {w, unit(t, "w2", "workload", "")},
		"no workload":                   {unit(t, "m", "mixin", "")},
	}
	for name, units := range tests {
		t.Run(name, func(t *testing.T) { _, err := resolve.Resolve(units); require.Error(t, err) })
	}
	t.Run("partial mixin set", func(t *testing.T) {
		r, err := resolve.ResolvePartial([]*resolve.Unit{unit(t, "m", "mixin", "")})
		require.NoError(t, err)
		require.Nil(t, r.Workload)
	})
	t.Run("integration orders", func(t *testing.T) {
		a := unit(t, "z-provider", "mixin", "provides: [peer@1]")
		b := unit(t, "a-consumer", "mixin", "integrates: ['peer >= 1']")
		r, err := resolve.Resolve([]*resolve.Unit{b, a, w})
		require.NoError(t, err)
		require.Equal(t, []*resolve.Unit{a, b}, r.Mixins)
	})
	t.Run("credential owner", func(t *testing.T) {
		d := parse(t, capDocument("credential@1", capabilityConfigs["credential@1"]))
		w := &resolve.Unit{Reference: "w", Descriptor: d}
		m := unit(t, "m", "mixin", "")
		m.Descriptor.Capabilities = d.Capabilities
		_, err := resolve.Resolve([]*resolve.Unit{w, m})
		require.ErrorContains(t, err, "one credential, one owner")
	})
	t.Run("version precedence", func(t *testing.T) {
		d := parse(t, document("version: '1.0.0'"))
		require.Equal(t, "2.0.0", resolve.EffectiveProvideVersion("example.invalid/kit:2.0.0", d))
		require.Equal(t, "1.0.0", resolve.EffectiveProvideVersion("./local", d))
	})
}

func TestMerge(t *testing.T) {
	_, w := fixture(t, "workload.yaml")
	_, m := fixture(t, "mixin.yaml")
	r, err := resolve.Resolve([]*resolve.Unit{{Reference: "m", Descriptor: m}, {Reference: "w", Descriptor: w}})
	require.NoError(t, err)
	var contributions []spec.Contribution
	for _, u := range r.Topological() {
		contributions = append(contributions, spec.Contribution{Reference: u.Reference, Descriptor: u.Descriptor})
	}
	result, err := spec.Merge(contributions, spec.MergeOptions{ContextPath: "/usr/share/sandbox/kit/merged/context.md"})
	require.NoError(t, err)
	d := result.Descriptor
	require.Equal(t, spec.KindWorkload, d.Kind)
	require.Empty(t, d.Requires)
	require.ElementsMatch(t, []string{"Apache-2.0", "MIT"}, d.Licenses)
	require.Len(t, d.Provides, 2)
	require.Len(t, result.ContextSources, 2)
	require.Equal(t, "w", result.ContextSources[0].Reference)
	lc, err := spec.LifecycleOf(d.Capabilities)
	require.NoError(t, err)
	require.Len(t, lc.Install, 2)
	require.Equal(t, "install", lc.Install[0].Command[1])
	require.Equal(t, "mixin", lc.Install[1].Command[1])
	_, err = spec.Validate(d)
	require.NoError(t, err)
	for _, tc := range []struct{ name, typ, a, b string }{
		{"file collision", "lifecycle@1", "{files: [{path: /same, content: a}]}", "{files: [{path: /same, content: b}]}"},
		{"interactive collision", "lifecycle@1", "{interactive: [a]}", "{interactive: [b]}"},
		{"resources collision", "resources@1", "{cpu: 1}", "{cpu: 2}"},
		{"volume conflicting config", "volume@1", "{path: /same, size: 1g}", "{path: /same, size: 2g}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := parse(t, capDocument(tc.typ, tc.a))
			b := parse(t, capDocument(tc.typ, tc.b))
			b.Kind = spec.KindMixin
			_, err := spec.Merge([]spec.Contribution{{Reference: "a", Descriptor: a}, {Reference: "b", Descriptor: b}}, spec.MergeOptions{})
			require.Error(t, err)
		})
	}
	t.Run("network version union", func(t *testing.T) {
		a := parse(t, capDocument("network-policy@1", "{runtime: {allow: [api.example.invalid], deny: [deny.example.invalid]}}"))
		b := parse(t, capDocument("network-policy@2", capabilityConfigs["network-policy@2"]))
		b.Kind = spec.KindMixin
		merged, err := spec.Merge([]spec.Contribution{{Reference: "a", Descriptor: a}, {Reference: "b", Descriptor: b}}, spec.MergeOptions{})
		require.NoError(t, err)
		require.True(t, spec.HasCapability(merged.Descriptor.Capabilities, spec.CapabilityNetworkPolicyV2))
		p, err := spec.NetworkPolicyV2Of(merged.Descriptor.Capabilities)
		require.NoError(t, err)
		require.Len(t, p.Runtime.Allow, 1)
		require.False(t, p.Runtime.Allow[0].Bounded())
		require.Len(t, p.Runtime.Deny, 2)
	})
	t.Run("identical resource dedup", func(t *testing.T) {
		a := parse(t, capDocument("resources@1", "{cpu: 1}"))
		b := parse(t, capDocument("resources@1", "{cpu: 1}"))
		b.Kind = spec.KindMixin
		merged, err := spec.Merge([]spec.Contribution{{Reference: "a", Descriptor: a}, {Reference: "b", Descriptor: b}}, spec.MergeOptions{})
		require.NoError(t, err)
		require.Len(t, merged.Descriptor.Capabilities, 1)
	})
	t.Run("reexport contract", func(t *testing.T) {
		original := spec.Arg{Required: true, Enum: []string{"alpha", "beta"}}
		require.NoError(t, spec.CheckReExport("team", original, "team", spec.Arg{Required: true, Enum: []string{"alpha"}}, true))
		require.Error(t, spec.CheckReExport("team", original, "team", spec.Arg{}, true))
	})
}

func TestPermissionSurface(t *testing.T) {
	for _, tc := range []struct{ name, typ, narrow, wide string }{
		{"network allow", "network-policy@1", "{runtime: {allow: [a.example.invalid]}}", "{runtime: {allow: [a.example.invalid, b.example.invalid]}}"},
		{"deny removal", "network-policy@1", "{runtime: {allow: ['*'], deny: [a.example.invalid]}}", "{runtime: {allow: ['*']}}"},
		{"http unbound", "network-policy@2", "{runtime: {allow: [{hosts: [api.example.invalid], methods: [GET], paths: ['/v1/**']}]}}", "{runtime: {allow: [api.example.invalid]}}"},
		{"skills write", "agent-skills@1", "{path: /skills}", "{path: /skills, mode: readwrite}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			narrow := spec.SurfaceOf(parse(t, capDocument(tc.typ, tc.narrow)))
			wide := spec.SurfaceOf(parse(t, capDocument(tc.typ, tc.wide)))
			require.NotEmpty(t, spec.DiffWidenings(narrow, wide))
			require.Empty(t, spec.DiffWidenings(wide, narrow))
		})
	}
	for _, typ := range []string{"resources@1", "lifecycle@1", "agent-context@1", "agent-sessions@1", "sbx@1"} {
		t.Run("not grant "+typ, func(t *testing.T) {
			require.Empty(t, spec.DiffWidenings(spec.Surface{}, spec.SurfaceOf(parse(t, capDocument(typ, capabilityConfigs[typ])))))
		})
	}
	for _, typ := range []string{"credential@1", "volume@1", "port@1", "usb-device@1", "privileged@1", "kit-registry@1"} {
		t.Run("grant "+typ, func(t *testing.T) {
			require.NotEmpty(t, spec.DiffWidenings(spec.Surface{}, spec.SurfaceOf(parse(t, capDocument(typ, capabilityConfigs[typ])))))
		})
	}
	t.Run("opaque extension", func(t *testing.T) {
		raw := document("capabilities: [{type: org.example/extension@1, optional: true, config: {arbitrary: yes}}]")
		d := parse(t, raw)
		surface := spec.SurfaceOf(d)
		require.Len(t, surface.Services, 1)
		d.Capabilities[0].Optional = false
		require.Equal(t, surface, spec.SurfaceOf(d))
		d.Capabilities[0].Config["arbitrary"] = "changed"
		require.NotEmpty(t, spec.DiffWidenings(surface, spec.SurfaceOf(d)))
	})
}

func TestLock(t *testing.T) {
	w := unit(t, "workload", "workload", "")
	w.Args = map[string]string{"synthetic": "value"}
	r, err := resolve.Resolve([]*resolve.Unit{w})
	require.NoError(t, err)
	lock := resolve.LockFrom(r)
	bytes, err := lock.Marshal()
	require.NoError(t, err)
	restored, err := resolve.ParseLock(bytes)
	require.NoError(t, err)
	require.Equal(t, lock, restored)
	require.NoError(t, restored.VerifyRecreate(r))
	require.Empty(t, restored.Gate(r))
	w.Digest = "sha256:" + strings.Repeat("b", 64)
	require.Error(t, restored.VerifyRecreate(r))
	_, err = resolve.ParseLock([]byte(`{"version":999}`))
	require.Error(t, err)
}

func TestDerivedPackages(t *testing.T) {
	dpkg := "Package: installed\nStatus: install ok installed\nVersion: 1:2.5.2-3+dhi1\n\nPackage: removed\nStatus: deinstall ok config-files\nVersion: 9.0\n\nPackage: prerelease\nStatus: install ok installed\nVersion: 2.0~rc1-1\n\n"
	packages, err := spec.ReadDpkgStatus(strings.NewReader(dpkg))
	require.NoError(t, err)
	provides := spec.DerivedProvides("deb", [][]spec.Package{packages})
	require.Equal(t, []string{"deb/installed@2.5.2"}, provides)
	apk, err := spec.ReadApkInstalled(strings.NewReader("P:busybox\nV:1.37.0-r18\n\n"))
	require.NoError(t, err)
	require.Equal(t, []string{"apk/busybox@1.37.0"}, spec.DerivedProvides("apk", [][]spec.Package{apk}))
	require.Empty(t, spec.DerivedProvides("deb", [][]spec.Package{packages, {}}))
	for raw, want := range map[string]string{"1:2.5.2-3+dhi1": "2.5.2", "9.20.26-1~deb13u1": "9.20.26", "2.0~rc1": "", "1.37.0-r18": "1.37.0"} {
		t.Run(raw, func(t *testing.T) { require.Equal(t, want, spec.PackageVersion(raw)) })
	}
}
