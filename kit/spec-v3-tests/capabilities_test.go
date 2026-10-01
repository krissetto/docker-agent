package specv3_test

import (
	"os"
	"testing"

	"github.com/docker/sandbox-kit-spec/v3/spec"
	"github.com/stretchr/testify/require"
)

// Each family is validated with its official typed decoder, not an opaque extension.
var capabilityConfigs = map[string]string{
	"network-policy@1": "{install: {allow: ['*.example.invalid']}, runtime: {allow: [api.example.invalid], deny: [deny.example.invalid]}}",
	"network-policy@2": "{runtime: {allow: [{hosts: [api.example.invalid], methods: [GET, HEAD], paths: ['/v1/**']}], deny: [{hosts: [api.example.invalid], methods: [DELETE]}]}}",
	"credential@1":     "{service: synthetic, phase: runtime, apiKey: {name: SYNTHETIC_TOKEN, proxyManaged: true}, oauth: {tokenEndpoint: {host: auth.example.invalid, path: /token}, resourceHosts: [api.example.invalid], sentinels: {accessToken: dummy-access, refreshToken: dummy-refresh}, credentialFile: {path: ~/.fixture/credentials.json, structure: {token: '{{.AccessToken}}', expires: '{{.ExpiresAt}}', scopes: '{{.Scopes}}', key: '{{.PrimaryApiKey}}'}}, responseFields: {accessToken: token, refreshToken: refresh, expiresIn: expires}, passthrough: false}}",
	"volume@1":         "{path: /home/agent/state, size: 2g, mode: '0755', tmpfs: false}",
	"port@1":           "{container: 8080, transport: tcp, name: synthetic}",
	"usb-device@1":     "{vendorId: '0000', productId: '0000'}",
	"resources@1":      "{cpu: 1.5, memory: 512m, gpu: '1'}",
	"privileged@1":     "",
	"lifecycle@1":      "{install: [{command: [echo, install], env: [HOME]}], startup: [{command: 'echo startup', background: true}], files: [{path: /home/agent/file, content: synthetic, mode: '0600', overwrite: false}], interactive: [--tty]}",
	"agent-context@1":  "{filename: AGENTS.md, content: synthetic}",
	"agent-sessions@1": "{prompt: ['--prompt={{.Prompt}}'], resume: [--resume, '{{.SessionID}}'], continue: [--continue], list: 'echo synthetic-session'}",
	"agent-skills@1":   "{path: /home/agent/skills, mode: readonly}",
	"kit-registry@1":   "",
	"sbx@1":            "",
}

func TestAllCapabilityTypes(t *testing.T) {
	require.Len(t, capabilityConfigs, 14)
	for typ, config := range capabilityConfigs {
		t.Run(typ, func(t *testing.T) {
			d := parse(t, capDocument(typ, config))
			require.Equal(t, "com.docker.sandbox/"+typ, spec.CapabilityTypes(d.Capabilities))
			_, err := spec.ValidatePublished([]byte(capDocument(typ, config)), d)
			require.NoError(t, err)
			if config != "" {
				d.Capabilities[0].Config["inventedField"] = true
				_, err := spec.Validate(d)
				require.Error(t, err, "well-known configs must not be opaque")
			}
		})
	}
}

func TestCapabilityNegatives(t *testing.T) {
	cases := []struct{ name, typ, config string }{
		{"relative volume", "volume@1", "{path: relative}"},
		{"volume mode", "volume@1", "{path: /state, mode: '0999'}"},
		{"volume size", "volume@1", "{path: /state, size: huge}"},
		{"port zero", "port@1", "{container: 0}"},
		{"port large", "port@1", "{container: 65536}"},
		{"port protocol", "port@1", "{container: 80, transport: sctp}"},
		{"cpu negative", "resources@1", "{cpu: -1}"},
		{"memory invalid", "resources@1", "{memory: huge}"},
		{"usb exclusive", "usb-device@1", "{vendorId: '0000', productId: '0000', class: smart-card}"},
		{"usb partner", "usb-device@1", "{vendorId: '0000'}"},
		{"usb empty", "usb-device@1", "{}"},
		{"skills alias", "agent-skills@1", "{path: /x/../skills}"},
		{"skills root", "agent-skills@1", "{path: /}"},
		{"skills mode", "agent-skills@1", "{path: /skills, mode: write}"},
		{"empty lifecycle", "lifecycle@1", "{}"},
		{"hook command", "lifecycle@1", "{install: [{env: [HOME]}]}"},
		{"hook env", "lifecycle@1", "{install: [{command: echo, env: [BAD-NAME]}]}"},
		{"file relative", "lifecycle@1", "{files: [{path: relative, content: x}]}"},
		{"file mode", "lifecycle@1", "{files: [{path: /x, mode: '0888'}]}"},
		{"context exclusive", "agent-context@1", "{content: x, contentFile: ./x}"},
		{"empty sessions", "agent-sessions@1", "{}"},
		{"prompt placeholder", "agent-sessions@1", "{prompt: [--prompt]}"},
		{"resume placeholder", "agent-sessions@1", "{resume: [--resume]}"},
		{"credential presentation", "credential@1", "{service: synthetic, phase: runtime}"},
		{"credential phase", "credential@1", "{service: synthetic, phase: boot, apiKey: {name: TOKEN}}"},
		{"credential empty key", "credential@1", "{service: synthetic, phase: runtime, apiKey: {}}"},
		{"credential null", "credential@1", "{service: synthetic, phase: runtime, apiKey: null}"},
		{"oauth endpoint", "credential@1", "{service: synthetic, phase: runtime, oauth: {tokenEndpoint: {path: /token}}}"},
		{"oauth encoding", "credential@1", "{service: synthetic, phase: runtime, oauth: {credentialFile: {path: /token, format: xml}}}"},
		{"toml null", "credential@1", "{service: synthetic, phase: runtime, oauth: {credentialFile: {path: /token, format: toml, structure: {token: null}}}}"},
		{"http lowercase", "network-policy@2", "{runtime: {allow: [{hosts: [api.example.invalid], methods: [get]}]}}"},
		{"http empty methods", "network-policy@2", "{runtime: {allow: [{hosts: [api.example.invalid], methods: []}]}}"},
		{"http paths require methods", "network-policy@2", "{runtime: {allow: [{hosts: [api.example.invalid], paths: ['/v1/**']}]}}"},
		{"http bounded wildcard", "network-policy@2", "{runtime: {allow: [{hosts: ['*.example.invalid'], methods: [GET]}]}}"},
		{"http any exclusive", "network-policy@2", "{runtime: {allow: [{hosts: [api.example.invalid], methods: [ANY, GET]}]}}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { require.Error(t, validationError(capDocument(tc.typ, tc.config))) })
	}
	for _, typ := range []string{"sbx@1", "privileged@1", "kit-registry@1"} {
		for _, config := range []string{"{}", "null"} {
			t.Run(typ+config, func(t *testing.T) { require.Error(t, validationError(capDocument(typ, config))) })
		}
	}
	for _, typ := range []string{"agent-context@1", "sbx@1"} {
		t.Run("mixin "+typ, func(t *testing.T) {
			raw := "schemaVersion: \"3\"\nkind: mixin\ncapabilities:\n- type: com.docker.sandbox/" + typ + "\n"
			if typ == "agent-context@1" {
				raw += "  config: {filename: AGENTS.md}\n"
			}
			require.Error(t, validationError(raw))
		})
	}
}

func TestCapabilityArity(t *testing.T) {
	for typ, config := range capabilityConfigs {
		t.Run(typ, func(t *testing.T) {
			d := parse(t, capDocument(typ, config))
			d.Capabilities = append(d.Capabilities, d.Capabilities[0])
			_, err := spec.Validate(d)
			require.Error(t, err)
		})
	}
	t.Run("network versions exclusive", func(t *testing.T) {
		d := parse(t, capDocument("network-policy@1", capabilityConfigs["network-policy@1"]))
		d.Capabilities = append(d.Capabilities, parse(t, capDocument("network-policy@2", capabilityConfigs["network-policy@2"])).Capabilities...)
		_, err := spec.Validate(d)
		require.Error(t, err)
	})
	for _, tc := range []struct{ typ, first, second string }{
		{"volume@1", "{path: /state, size: 1g}", "{path: /state, size: 2g}"},
		{"port@1", "{container: 80}", "{container: 80, transport: tcp}"},
		{"agent-skills@1", "{path: /skills}", "{path: /skills, mode: readwrite}"},
		{"credential@1", "{service: synthetic, phase: runtime, apiKey: {name: ONE}}", "{service: synthetic, phase: runtime, apiKey: {name: TWO}}"},
	} {
		t.Run("key "+tc.typ, func(t *testing.T) {
			d := parse(t, capDocument(tc.typ, tc.first))
			d.Capabilities = append(d.Capabilities, parse(t, capDocument(tc.typ, tc.second)).Capabilities...)
			_, err := spec.Validate(d)
			require.Error(t, err)
		})
	}
}

func TestWorkloadCredentialContract(t *testing.T) {
	raw, err := os.ReadFile("../async-agent.yaml")
	require.NoError(t, err)
	d := parse(t, string(raw))
	credentials, err := spec.CredentialsOfPhase(d.Capabilities, "runtime")
	require.NoError(t, err)
	require.Len(t, credentials, 4)
	required := map[string]bool{}
	for _, credential := range credentials {
		required[credential.Service] = credential.Required
		if credential.Service == "github" {
			require.NotNil(t, credential.APIKey)
			require.Equal(t, "GH_TOKEN", credential.APIKey.Name)
			require.True(t, credential.APIKey.ProxyManaged)
			require.Len(t, credential.APIKey.Inject, 2)
		}
	}
	require.Equal(t, map[string]bool{"openai": true, "anthropic": true, "google": false, "github": false}, required)
	install, err := spec.CredentialsOfPhase(d.Capabilities, "install")
	require.NoError(t, err)
	require.Empty(t, install)

	for _, missing := range []string{"api.github.com", "github.com"} {
		t.Run("missing runtime route "+missing, func(t *testing.T) {
			d := parse(t, string(raw))
			for i := range d.Capabilities {
				if d.Capabilities[i].Type != spec.CapabilityNetworkPolicy {
					continue
				}
				allow := []string{"api.openai.com", "api.anthropic.com", "generativelanguage.googleapis.com"}
				for _, host := range []string{"api.github.com", "github.com"} {
					if host != missing {
						allow = append(allow, host)
					}
				}
				d.Capabilities[i].Config = map[string]any{"runtime": map[string]any{"allow": allow}}
			}
			_, err := spec.Validate(d)
			require.Error(t, err, "credential injection requires the matching runtime route")
		})
	}
}

func TestCredentialNetworkBinding(t *testing.T) {
	for _, version := range []string{"1", "2"} {
		for _, phase := range []string{"install", "runtime"} {
			t.Run(version+phase, func(t *testing.T) {
				raw := document("capabilities:\n- type: com.docker.sandbox/network-policy@" + version + "\n  config: {" + phase + ": {allow: [api.example.invalid:443]}}\n- type: com.docker.sandbox/credential@1\n  config: {service: synthetic, phase: " + phase + ", apiKey: {proxyManaged: true, inject: [{domain: api.example.invalid, header: Authorization, format: 'Bearer %s'}, {domain: api.example.invalid, scheme: basic, username: synthetic}]}}")
				d := parse(t, raw)
				creds, err := spec.CredentialsOfPhase(d.Capabilities, phase)
				require.NoError(t, err)
				require.Len(t, creds, 1)
				require.Empty(t, creds[0].APIKey.Name)
				d.Capabilities[1].Config["phase"] = map[string]string{"install": "runtime", "runtime": "install"}[phase]
				_, err = spec.Validate(d)
				require.Error(t, err)
			})
		}
	}
	for name, config := range map[string]string{
		"usb class": "{class: smart-card}",
	} {
		t.Run(name, func(t *testing.T) { parse(t, capDocument("usb-device@1", config)) })
	}
	t.Run("oauth toml passthrough", func(t *testing.T) {
		parse(t, capDocument("credential@1", "{service: synthetic, phase: runtime, oauth: {passthrough: true, credentialFile: {path: /home/agent/token.toml, format: toml, structure: {token: '{{.AccessToken}}', scopes: '{{.Scopes}}'}}}}"))
	})
	t.Run("tmpfs and udp", func(t *testing.T) {
		parse(t, capDocument("volume@1", "{path: /scratch, tmpfs: true, mode: '1777'}"))
		parse(t, capDocument("port@1", "{container: 5353, transport: udp}"))
	})
}
