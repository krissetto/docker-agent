package asyncsubagents_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type kitBuildCall struct {
	Tool string   `json:"tool"`
	Args []string `json:"args"`
	Cwd  string   `json:"cwd"`
}
type kitBuildFixture struct{ root, log, task string }

func newKitBuildFixture(t *testing.T) kitBuildFixture {
	t.Helper()
	task, err := exec.LookPath("task")
	if err != nil {
		t.Skip("Task binary is not installed")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python 3 is required for isolated OCI stubs")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(root, "kit", path), 0700)
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(root, "kit", path), data, 0700)
	}))
	data, err := os.ReadFile("../Taskfile.yml")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "Taskfile.yml"), data, 0600))
	bin := filepath.Join(root, "bin")
	require.NoError(t, os.Mkdir(bin, 0700))
	for _, tool := range []string{"docker", "oras"} {
		require.NoError(t, os.WriteFile(filepath.Join(bin, tool), []byte("#!"+python+"\n"+kitBuildStub), 0700))
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KIT_TEST_LOG", filepath.Join(root, "calls.jsonl"))
	t.Setenv("KIT_TEST_ROOT", root)
	t.Setenv("KIT_PLATFORM", "")
	for _, tool := range []string{"jq", "shasum", "bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is required for the build helper", tool)
		}
	}
	t.Setenv("DOCKER_DEFAULT_PLATFORM", "")
	return kitBuildFixture{root, filepath.Join(root, "calls.jsonl"), task}
}

func (f kitBuildFixture) run(t *testing.T, task, ref, answer string, terminal bool) ([]kitBuildCall, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	args := []string{"--silent", task}
	if ref != "" {
		args = append(args, "--", ref)
	}
	cmd := exec.CommandContext(ctx, f.task, args...)
	cmd.Dir = f.root
	if terminal {
		master, slave, err := pty.Open()
		require.NoError(t, err)
		defer master.Close()
		defer slave.Close()
		cmd.Stdin = slave
		_, err = master.WriteString(answer)
		require.NoError(t, err)
	} else {
		cmd.Stdin = strings.NewReader(answer)
	}
	out, err := cmd.CombinedOutput()
	require.NotEqual(t, context.DeadlineExceeded, ctx.Err(), "build prompt hung: %s", out)
	var calls []kitBuildCall
	log, _ := os.ReadFile(f.log)
	for _, line := range bytes.Split(bytes.TrimSpace(log), []byte("\n")) {
		if len(line) > 0 {
			var call kitBuildCall
			require.NoError(t, json.Unmarshal(line, &call))
			calls = append(calls, call)
		}
	}
	return calls, string(out), err
}
func kitCalls(calls []kitBuildCall, tool, command string) []kitBuildCall {
	var result []kitBuildCall
	for _, c := range calls {
		if c.Tool == tool && len(c.Args) > 0 && c.Args[0] == command {
			result = append(result, c)
		}
	}
	return result
}
func kitArg(args []string, key string) string {
	if key == "--tag" {
		if value := kitArg(args, "-t"); value != "" {
			return value
		}
	}
	for i, arg := range args {
		if arg == key && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(arg, key+"=") {
			return strings.TrimPrefix(arg, key+"=")
		}
	}
	return ""
}

func TestKitBuildDefaultsAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		task, ref, answer string
		tty               bool
	}{
		{"kit", "docker.io/library/kagent:local-v3", "", false}, {"kit:build-v3", "docker.io/library/kagent:local-v3", "\n", true}, {"kit:build-v3", "docker.io/library/kagent:local-v3", "no\n", true}, {"kit:build-v3", "docker.io/library/kagent:local-v3", "yes\n", false}, {"kit:build-v2", "docker.io/library/kagent:local-v2-runtime", "", false},
	} {
		t.Run(tc.task+"/"+tc.answer, func(t *testing.T) {
			f := newKitBuildFixture(t)
			calls, out, err := f.run(t, tc.task, "", tc.answer, tc.tty)
			require.NoError(t, err, out)
			assert.Empty(t, kitCalls(calls, "oras", "cp"))
			assert.Empty(t, kitCalls(calls, "docker", "push"))
			builds := kitCalls(calls, "docker", "buildx")
			require.Len(t, builds, 2, out)
			for _, build := range builds {
				assert.Equal(t, "build", build.Args[1])
				assert.Equal(t, "linux/amd64", kitArg(build.Args, "--platform"))
				assert.Equal(t, tc.ref, kitArg(build.Args, "--tag"))
				assert.Equal(t, f.root, build.Cwd)
				assert.Contains(t, build.Args, f.root)
				if tc.task == "kit:build-v2" {
					assert.Equal(t, "runtime-v2", kitArg(build.Args, "--target"))
					assert.Equal(t, filepath.Join(f.root, "kit/async-agent.dockerfile"), kitArg(build.Args, "-f"))
				} else {
					assert.Equal(t, filepath.Join(f.root, "kit/async-agent.yaml"), kitArg(build.Args, "-f"))
				}
				assert.NotContains(t, build.Args, "--push")
			}
			assert.Contains(t, builds[0].Args, "--provenance=mode=min")
			assert.Contains(t, builds[1].Args, "--load")
			assert.Contains(t, builds[1].Args, "--provenance=false")
		})
	}
}

func TestKitBuildAliasNormalizesReference(t *testing.T) {
	for _, tc := range []struct {
		ref, effective string
	}{
		{"", "docker.io/library/kagent:local-v3"},
		{"kagent", "docker.io/library/kagent:latest"},
		{"kagent:chosen", "docker.io/library/kagent:chosen"},
		{"team/custom:chosen", "docker.io/team/custom:chosen"},
		{"team/custom", "docker.io/team/custom:latest"},
		{"docker.io/kagent:chosen", "docker.io/library/kagent:chosen"},
		{"docker.io/team/custom:chosen", "docker.io/team/custom:chosen"},
		{"ghcr.io/team/custom:chosen", "ghcr.io/team/custom:chosen"},
		{"localhost/custom:chosen", "localhost/custom:chosen"},
		{"localhost:5000/team/custom:chosen", "localhost:5000/team/custom:chosen"},
		{"registry:5000/custom:chosen", "registry:5000/custom:chosen"},
	} {
		t.Run(tc.effective+"/"+tc.ref, func(t *testing.T) {
			f := newKitBuildFixture(t)
			calls, out, err := f.run(t, "kit", tc.ref, "yes\n", true)
			require.NoError(t, err, out)
			builds := kitCalls(calls, "docker", "buildx")
			require.Len(t, builds, 2)
			for _, build := range builds {
				assert.Equal(t, tc.effective, kitArg(build.Args, "--tag"))
			}
			copies := kitCalls(calls, "oras", "cp")
			require.Len(t, copies, 1)
			assert.Equal(t, tc.effective, copies[0].Args[len(copies[0].Args)-1])
			assert.Contains(t, out, "Local Docker image: "+tc.effective)
			assert.Contains(t, out, "Built kit: "+tc.effective)
			assert.Contains(t, out, "Push "+tc.effective+"? [y/N]")
			repository := tc.effective[:strings.LastIndex(tc.effective, ":")]
			assert.Contains(t, out, "Immutable kit reference (after publication): "+repository+"@sha256:")
			assert.Contains(t, out, "Published kit: "+repository+"@sha256:")
		})
	}
}

func TestKitBuildRejectsUnsafeReferences(t *testing.T) {
	for _, ref := range []string{"--push", "repo:tag extra", "repo:tag; touch INJECTED", "repo:$(touch INJECTED)", "repo:`touch INJECTED`", "Repo:tag", "repo@sha256:abcd", "docker.io/Repo:tag", "docker.io/:tag", "localhost:bad/repo:tag", "team//repo:tag", "repo:" + strings.Repeat("a", 129)} {
		t.Run(ref, func(t *testing.T) {
			f := newKitBuildFixture(t)
			calls, out, err := f.run(t, "kit", ref, "", false)
			require.Error(t, err, out)
			assert.Empty(t, calls)
			assert.NoDirExists(t, filepath.Join(f.root, "dist"))
			assert.NoFileExists(t, filepath.Join(f.root, "INJECTED"))
		})
	}
	t.Run("runtime tag overflow", func(t *testing.T) {
		f := newKitBuildFixture(t)
		calls, out, err := f.run(t, "kit:build-v2", "repo:"+strings.Repeat("a", 121), "", false)
		require.Error(t, err, out)
		assert.Empty(t, calls)
	})
}

func TestKitBuildRejectsMultipleArguments(t *testing.T) {
	f := newKitBuildFixture(t)
	cmd := exec.CommandContext(t.Context(), f.task, "--silent", "kit", "--", "repo:tag", "second:tag")
	cmd.Dir = f.root
	out, err := cmd.CombinedOutput()
	require.Error(t, err, string(out))
	assert.NoFileExists(t, f.log)
}

func TestKitBuildMissingAnnotationsDoesNotPublish(t *testing.T) {
	f := newKitBuildFixture(t)
	t.Setenv("KIT_TEST_MISSING_ANNOTATIONS", "1")
	calls, out, err := f.run(t, "kit", "", "yes\n", true)
	require.Error(t, err, out)
	assert.Empty(t, kitCalls(calls, "oras", "cp"))
}

func TestKitBuildFailureDoesNotPublish(t *testing.T) {
	f := newKitBuildFixture(t)
	t.Setenv("KIT_TEST_FAIL_BUILD", "1")
	calls, out, err := f.run(t, "kit", "", "yes\n", true)
	require.Error(t, err, out)
	assert.Empty(t, kitCalls(calls, "oras", "cp"))
	assert.Empty(t, kitCalls(calls, "docker", "push"))
}

func TestKitBuildV3PublishedRoot(t *testing.T) {
	f := newKitBuildFixture(t)
	t.Setenv("KIT_PLATFORM", "linux/arm64")
	ref := "localhost:5000/team/kit:test"
	calls, out, err := f.run(t, "kit:build-v3", ref, "YES\n", true)
	require.NoError(t, err, out)
	copies := kitCalls(calls, "oras", "cp")
	require.Len(t, copies, 1)
	assert.Equal(t, ref, copies[0].Args[len(copies[0].Args)-1])
	assert.Contains(t, copies[0].Args, "--from-oci-layout")
	assert.Empty(t, kitCalls(calls, "docker", "push"))
	for _, build := range kitCalls(calls, "docker", "buildx") {
		assert.Equal(t, "linux/arm64", kitArg(build.Args, "--platform"))
	}
	var snap struct {
		Root     map[string]any `json:"root"`
		Original map[string]any `json:"original"`
		Valid    bool           `json:"valid"`
	}
	data, err := os.ReadFile(filepath.Join(f.root, "snapshot-1.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &snap))
	assert.True(t, snap.Valid, "selected root descriptor must match actual blob bytes")
	assert.Equal(t, snap.Original["manifests"], snap.Root["manifests"], "all platform and provenance descriptors survive unchanged")
	annotations := snap.Root["annotations"].(map[string]any)
	for _, key := range []string{"vnd.docker.sandbox.kit.descriptor", "vnd.docker.sandbox.kit.schema-version", "vnd.docker.sandbox.kit.capabilities"} {
		assert.Equal(t, snap.Original["manifests"].([]any)[0].(map[string]any)["annotations"].(map[string]any)[key], annotations[key])
	}
	assert.Equal(t, "keep-root", annotations["test.root"])
}

func TestKitBuildV2ArtifactAndPublicationOrder(t *testing.T) {
	for _, tc := range []struct {
		name, ref, repository, tag string
		fail                       bool
	}{
		{"approved", "localhost:5000/team/kit:trial", "localhost:5000/team/kit", "trial", false},
		{"runtime failure", "localhost:5000/team/kit:trial", "localhost:5000/team/kit", "trial", true},
		{"Docker Hub namespace", "team/kit:trial", "docker.io/team/kit", "trial", false},
		{"Docker Hub latest", "team/kit", "docker.io/team/kit", "latest", false},
		{"Docker Hub default", "", "docker.io/library/kagent", "local-v2", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newKitBuildFixture(t)
			if tc.fail {
				t.Setenv("KIT_TEST_FAIL_COPY", "1")
			}
			before, err := os.ReadFile(filepath.Join(f.root, "kit/v2/spec.yaml"))
			require.NoError(t, err)
			calls, out, err := f.run(t, "kit:build-v2", tc.ref, "y\n", true)
			if tc.fail {
				require.Error(t, err, out)
			} else {
				require.NoError(t, err, out)
			}
			after, err := os.ReadFile(filepath.Join(f.root, "kit/v2/spec.yaml"))
			require.NoError(t, err)
			assert.Equal(t, before, after)
			pushes := kitCalls(calls, "oras", "push")
			require.Len(t, pushes, 1)
			args := pushes[0].Args
			assert.Contains(t, args, "--oci-layout")
			assert.Equal(t, "v1.1", kitArg(args, "--image-spec"))
			assert.Equal(t, "application/vnd.docker.sandbox.kit.v2", kitArg(args, "--artifact-type"))
			assert.True(t, strings.HasSuffix(kitArg(args, "--config"), ":application/vnd.docker.sandbox.kit.v2.spec+yaml"))
			spec, err := os.ReadFile(filepath.Join(f.root, "packed-spec.yaml"))
			require.NoError(t, err)
			var parsed struct {
				Sandbox struct {
					Image string `yaml:"image"`
				} `yaml:"sandbox"`
			}
			require.NoError(t, yaml.Unmarshal(spec, &parsed))
			digest, err := os.ReadFile(filepath.Join(f.root, "runtime-digest"))
			require.NoError(t, err)
			assert.Equal(t, tc.repository+"@"+string(digest), parsed.Sandbox.Image)
			builds := kitCalls(calls, "docker", "buildx")
			require.Len(t, builds, 2)
			for _, build := range builds {
				assert.Equal(t, tc.repository+":"+tc.tag+"-runtime", kitArg(build.Args, "--tag"))
			}
			assert.Contains(t, out, "Runtime: "+tc.repository+":"+tc.tag+"-runtime ("+parsed.Sandbox.Image+")")
			assert.Contains(t, out, "Built kit: "+tc.repository+":"+tc.tag)
			assert.Contains(t, out, "Immutable kit reference (after publication): "+tc.repository+"@sha256:")
			copies := kitCalls(calls, "oras", "cp")
			want := 2
			if tc.fail {
				want = 1
			}
			require.Len(t, copies, want)
			assert.Equal(t, tc.repository+":"+tc.tag+"-runtime", copies[0].Args[len(copies[0].Args)-1])
			if !tc.fail {
				assert.Equal(t, tc.repository+":"+tc.tag, copies[1].Args[len(copies[1].Args)-1])
			}
			assert.Empty(t, kitCalls(calls, "docker", "push"))
		})
	}
}

// Only Docker and ORAS are replaced. The real Task shell quoting, build helper,
// OCI metadata transformation, and confirmation gate execute without a daemon.
const kitBuildStub = `import hashlib,json,os,pathlib,shutil,sys,tarfile
p=pathlib.Path
args=sys.argv[1:]; tool=p(sys.argv[0]).name; home=p(os.environ['KIT_TEST_ROOT'])
with open(os.environ['KIT_TEST_LOG'],'a') as f: f.write(json.dumps(dict(tool=tool,args=args,cwd=os.getcwd()))+'\n')
def opt(key):
 for i,a in enumerate(args):
  if a==key: return args[i+1]
  if a.startswith(key+'='): return a[len(key)+1:]
 return ''
def blob(layout,obj):
 b=json.dumps(obj,separators=(',',':')).encode(); digest=hashlib.sha256(b).hexdigest(); path=layout/'blobs/sha256'/digest; path.parent.mkdir(parents=True,exist_ok=True);path.write_bytes(b)
 return dict(mediaType=obj.get('mediaType','application/vnd.oci.image.config.v1+json'),digest='sha256:'+digest,size=len(b))
def fixture(layout):
 layout.mkdir(parents=True,exist_ok=True); (layout/'oci-layout').write_text('{"imageLayoutVersion":"1.0.0"}')
 annotations={'vnd.docker.sandbox.kit.descriptor':'schemaVersion: "3"\nkind: workload\n','vnd.docker.sandbox.kit.schema-version':'3','vnd.docker.sandbox.kit.capabilities':'["com.docker.sandbox/sbx@1"]','test.child':'keep-child'}
 if os.environ.get('KIT_TEST_MISSING_ANNOTATIONS'):annotations.pop('vnd.docker.sandbox.kit.capabilities')
 manifests=[]
 platforms=opt('--platform').split(',')
 if os.environ.get('KIT_TEST_MISSING_PLATFORM'): platforms=platforms[:1]
 for i,platform in enumerate(platforms):
  parts=platform.split('/');arch=parts[1]
  config=blob(layout,dict(architecture=arch,os='linux',rootfs=dict(type='layers',diff_ids=[])))
  current=dict(annotations)
  if i and os.environ.get('KIT_TEST_MISMATCH_ANNOTATIONS'):current['vnd.docker.sandbox.kit.schema-version']='different'
  manifest=dict(schemaVersion=2,mediaType='application/vnd.oci.image.manifest.v1+json',config=config,layers=[],annotations=current)
  child=blob(layout,manifest);child['platform']=dict(os='linux',architecture=arch)
  if len(parts)>2:child['platform']['variant']=parts[2]
  child['annotations']=current
  provenance=blob(layout,dict(manifest,annotations={'test.provenance':'keep'}));provenance['platform']=dict(os='unknown',architecture='unknown');provenance['annotations']={'vnd.docker.reference.type':'attestation-manifest','vnd.docker.reference.digest':child['digest']}
  manifests.extend([child,provenance])
 root=dict(schemaVersion=2,mediaType='application/vnd.oci.image.index.v1+json',manifests=manifests,annotations={'test.root':'keep-root'})
 desc=blob(layout,root);desc['annotations']={'org.opencontainers.image.ref.name':'latest'}
 (layout/'index.json').write_text(json.dumps(dict(schemaVersion=2,manifests=[desc])))
 (home/'original.json').write_text(json.dumps(root));(home/'runtime-digest').write_text(desc['digest'])
if tool=='docker':
 if args[:2]==['buildx','build']:
  if os.environ.get('KIT_TEST_FAIL_BUILD'): sys.exit(7)
  output=opt('--output') or opt('-o')
  if output:
   fields=dict(s.split('=',1) for s in output.split(',') if '=' in s);dest=p(fields['dest'])
   if fields.get('tar')=='false':fixture(dest)
   else:
    temp=home/'export-layout';fixture(temp)
    with tarfile.open(dest,'w') as f:
     for path in temp.iterdir(): f.add(path,arcname=path.name)
  sys.exit(0)
 if args==['buildx','version'] or args==['version']:sys.exit(0)
 sys.exit('unexpected docker invocation')
if args[0]=='version':sys.exit(0)
if args[0]=='push':
 assert '--oci-layout' in args
 config=opt('--config').rsplit(':',1)[0];shutil.copyfile(config,home/'packed-spec.yaml')
 source=next(a for a in args[1:] if a.endswith(':kit'));layout=p(source.rsplit(':',1)[0]);layout.mkdir(parents=True,exist_ok=True)
 raw=p(config).read_bytes();digest=hashlib.sha256(raw).hexdigest();path=layout/'blobs/sha256'/digest;path.parent.mkdir(parents=True,exist_ok=True);path.write_bytes(raw)
 conf=dict(mediaType='application/vnd.docker.sandbox.kit.v2.spec+yaml',digest='sha256:'+digest,size=len(raw))
 desc=blob(layout,dict(schemaVersion=2,mediaType='application/vnd.oci.image.manifest.v1+json',artifactType='application/vnd.docker.sandbox.kit.v2',config=conf,layers=[]));desc['annotations']={'org.opencontainers.image.ref.name':'kit'}
 (layout/'index.json').write_text(json.dumps(dict(schemaVersion=2,manifests=[desc])));(layout/'oci-layout').write_text('{"imageLayoutVersion":"1.0.0"}')
 print('Digest: '+desc['digest']);sys.exit(0)
if args[0]=='cp':
 n=len(list(home.glob('snapshot-*.json')))+1
 source=args[-2];layout=p(source.rsplit(':',1)[0]);selector=source.rsplit(':',1)[1];idx=json.loads((layout/'index.json').read_text());desc=next(d for d in idx['manifests'] if d.get('annotations',{}).get('org.opencontainers.image.ref.name')==selector)
 path=layout/'blobs/sha256'/desc['digest'].split(':')[1];data=path.read_bytes()
 (home/('snapshot-%d.json'%n)).write_text(json.dumps(dict(root=json.loads(data),original=json.loads((home/'original.json').read_text()),valid=desc['digest']=='sha256:'+hashlib.sha256(data).hexdigest() and desc['size']==len(data))))
 if os.environ.get('KIT_TEST_FAIL_COPY'):sys.exit(9)
 sys.exit(0)
sys.exit('unexpected oras invocation')
`

func TestKitBuildMultiPlatformPreservesAllManifestsWithoutDockerLoad(t *testing.T) {
	f := newKitBuildFixture(t)
	t.Setenv("KIT_PLATFORM", "linux/amd64,linux/arm64")
	calls, out, err := f.run(t, "kit:build-v3", "example.invalid/team/kagent:test", "yes\n", true)
	require.NoError(t, err, out)
	builds := kitCalls(calls, "docker", "buildx")
	require.Len(t, builds, 1)
	assert.Equal(t, "linux/amd64,linux/arm64", kitArg(builds[0].Args, "--platform"))
	assert.NotContains(t, builds[0].Args, "--load")
	assert.Contains(t, out, "not loaded into Docker")
	var snap struct {
		Root, Original map[string]any
		Valid          bool
	}
	data, err := os.ReadFile(filepath.Join(f.root, "snapshot-1.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &snap))
	assert.True(t, snap.Valid)
	assert.Equal(t, snap.Original["manifests"], snap.Root["manifests"])
	require.Len(t, snap.Root["manifests"], 4, "both target manifests and both attestations survive")
}

func TestKitMultiPlatformRejectsMissingOrInconsistentPlatform(t *testing.T) {
	for _, variable := range []string{"KIT_TEST_MISSING_PLATFORM", "KIT_TEST_MISMATCH_ANNOTATIONS"} {
		t.Run(variable, func(t *testing.T) {
			f := newKitBuildFixture(t)
			t.Setenv("KIT_PLATFORM", "linux/amd64,linux/arm64")
			t.Setenv(variable, "1")
			calls, out, err := f.run(t, "kit:build-v3", "example.invalid/team/kagent:test", "yes\n", true)
			require.Error(t, err, out)
			assert.Empty(t, kitCalls(calls, "oras", "cp"))
		})
	}
}

func TestKitPlatformListValidation(t *testing.T) {
	for _, platform := range []string{"linux/amd64,", "linux/amd64,,linux/arm64", ",linux/arm64", "linux/amd64,linux/amd64", "linux/amd64, linux/arm64", "darwin/arm64"} {
		t.Run(platform, func(t *testing.T) {
			f := newKitBuildFixture(t)
			t.Setenv("KIT_PLATFORM", platform)
			calls, out, err := f.run(t, "kit:build-v3", "", "", false)
			require.Error(t, err, out)
			assert.Empty(t, calls)
		})
	}
	f := newKitBuildFixture(t)
	t.Setenv("KIT_PLATFORM", "linux/amd64,linux/arm64")
	calls, out, err := f.run(t, "kit:build-v2", "", "", false)
	require.Error(t, err, out)
	assert.Empty(t, calls)
}
