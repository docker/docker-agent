package root

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/cli/cli/command"
	dockerconfig "github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/context/docker"
	"github.com/docker/cli/cli/context/store"
	cliflags "github.com/docker/cli/cli/flags"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/dmr"
	"github.com/docker/docker-agent/pkg/model/provider/dmr/dmrmodels"
)

func TestDMRDockerConnection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell Docker shim and Unix socket")
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		t.Skip("Desktop engine routing is host-only")
	}

	t.Setenv("MODEL_RUNNER_HOST", "")
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	configDir := t.TempDir()
	oldConfigDir := dockerconfig.Dir()
	dockerconfig.SetDir(configDir)
	t.Cleanup(func() { dockerconfig.SetDir(oldConfigDir) })
	t.Setenv("DOCKER_CONFIG", configDir)

	// A relative socket path keeps macOS's sockaddr_un path below its limit.
	t.Chdir(t.TempDir())
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "unix", "engine.sock")
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)
	host := "unix://" + filepath.Join(wd, "engine.sock")
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/exp/vDD4.40/engines/v1/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"ai/test"}]}`)
		case "/exp/vDD4.40/engines/_configure":
			w.WriteHeader(http.StatusAccepted)
		case "/exp/vDD4.40/engines/v1/embeddings":
			_, _ = io.WriteString(w, `{"data":[{"embedding":[0.1,0.2]}]}`)
		default:
			t.Errorf("unexpected engine request: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	require.NoError(t, server.Listener.Close())
	server.Listener = listener
	server.Start()
	defer server.Close()

	contexts := store.New(filepath.Join(configDir, "contexts"), command.DefaultContextStoreConfig())
	require.NoError(t, contexts.CreateOrUpdate(store.Metadata{
		Name:      "desktop-test",
		Endpoints: map[string]any{docker.DockerEndpoint: docker.EndpointMeta{Host: host}},
	}))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.json"), []byte(`{"currentContext":"unusable"}`), 0o600))

	shimDir := t.TempDir()
	argsFile := filepath.Join(shimDir, "args")
	t.Setenv("DMR_ARGS_FILE", argsFile)
	testBinary, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("DMR_DOCKER_HELPER", "1")
	script := "#!/bin/sh\nexec \"" + testBinary + "\" -test.run=^TestDMRDockerHelper$ -- \"$@\"\n"
	writeMockCLI(t, filepath.Join(shimDir, "docker"), script)
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	relativeConfig, err := filepath.Rel(wd, configDir)
	require.NoError(t, err)
	workspace := t.TempDir()
	for _, tt := range []struct {
		name, envName, envValue string
		args                    []string
	}{
		{name: "plugin context overrides environment", envName: "DOCKER_CONTEXT", envValue: "unusable", args: []string{"--context=desktop-test", "--config=" + configDir}},
		{name: "standalone DOCKER_CONTEXT", envName: "DOCKER_CONTEXT", envValue: "desktop-test"},
		{name: "standalone DOCKER_HOST", envName: "DOCKER_HOST", envValue: host},
		{name: "plugin host", args: []string{"--host=" + host}},
		{name: "standalone current context"},
		{name: "empty context pins current selection", args: []string{"--context="}},
		{name: "relative host with working directory", args: []string{"--host=unix://engine.sock"}},
		{name: "relative env host with working directory", envName: "DOCKER_HOST", envValue: "unix://engine.sock"},
		{name: "relative config with working directory", args: []string{"--context=desktop-test", "--config=" + relativeConfig}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envName != "" {
				t.Setenv(tt.envName, tt.envValue)
			}
			if tt.name == "standalone current context" || tt.name == "empty context pins current selection" {
				require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.json"), []byte(`{"currentContext":"desktop-test"}`), 0o600))
			}
			var dockerCLI command.Cli
			var flags *pflag.FlagSet
			if tt.args != nil {
				opts := cliflags.NewClientOptions()
				flags = pflag.NewFlagSet("docker", pflag.ContinueOnError)
				opts.InstallFlags(flags)
				require.NoError(t, flags.Parse(tt.args))
				opts.SetDefaultOptions(flags)
				cli, err := command.NewDockerCli(command.WithOutputStream(io.Discard), command.WithErrorStream(io.Discard))
				require.NoError(t, err)
				require.NoError(t, cli.Initialize(opts))
				dockerCLI = cli
			}

			ctx, err := withDMRDockerConnection(t.Context(), dockerCLI, flags)
			require.NoError(t, err)
			if strings.Contains(tt.name, "with working directory") {
				t.Chdir(wd) // Restore the process directory after setupWorkingDirectory.
				require.NoError(t, setupWorkingDirectory(&config.RuntimeConfig{WorkingDir: workspace}))
			}
			models, err := dmrmodels.ListModels(ctx)
			require.NoError(t, err)
			assert.Equal(t, []string{"ai/test"}, models)
			if tt.name == "standalone current context" || tt.name == "empty context pins current selection" {
				baseURL, transport := dmrmodels.ResolveBaseURL(ctx, nil, "http://model-runner.docker.internal/engines/v1/")
				require.NotNil(t, transport)
				defer transport.CloseIdleConnections()
				_, err := dmrmodels.ListModelsAt(t.Context(), transport, baseURL)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.json"), []byte(`{"currentContext":"unusable"}`), 0o600))
				transport.CloseIdleConnections()
				models, err := dmrmodels.ListModelsAt(t.Context(), transport, baseURL)
				require.NoError(t, err)
				assert.Equal(t, []string{"ai/test"}, models)
			}
			gotArgs, err := os.ReadFile(argsFile)
			require.NoError(t, err)
			for _, arg := range tt.args {
				if arg == "--context=" {
					arg = "--context=desktop-test"
				}
				if arg == "--host=unix://engine.sock" {
					arg = "--host=" + host
				}
				if strings.HasPrefix(arg, "--config=") {
					arg = "--config=" + configDir
				}
				assert.Contains(t, strings.Split(string(gotArgs), "\n"), arg)
			}
			assert.True(t, strings.HasSuffix(string(gotArgs), "model\nstatus\n--json\n"))

			var doctor doctorFlags
			withDoctorTestEnv(nil, nil, nil)(&doctor)
			doctor.dmrLister = nil
			report, err := doctor.buildReport(ctx, "")
			require.NoError(t, err)
			assert.Equal(t, dmrStatusReachable, report.DMR.Status)
			assert.Empty(t, report.Issues)

			client, err := dmr.NewClient(ctx, &latest.ModelConfig{Provider: "dmr", Model: "ai/test"})
			require.NoError(t, err)
			_, err = client.CreateBatchEmbedding(t.Context(), []string{"hello"})
			require.NoError(t, err)
		})
	}
}

func TestDMRDockerConnectionOverridesRemainLazy(t *testing.T) {
	t.Setenv("DOCKER_CONTEXT", "does-not-exist")
	ctx, err := withDMRDockerConnection(t.Context(), nil, nil)
	require.NoError(t, err)
	baseURL, client := dmrmodels.ResolveBaseURL(ctx, &latest.ModelConfig{BaseURL: "http://explicit/"}, "")
	assert.Equal(t, "http://explicit/", baseURL)
	assert.Nil(t, client)

	t.Setenv("MODEL_RUNNER_HOST", "http://runner")
	baseURL, client = dmrmodels.ResolveBaseURL(ctx, nil, "")
	assert.Equal(t, "http://runner/engines/v1/", baseURL)
	assert.Nil(t, client)
}

func TestDMRDockerConnectionForwardsTLSFlags(t *testing.T) {
	opts := cliflags.NewClientOptions()
	flags := pflag.NewFlagSet("docker", pflag.ContinueOnError)
	opts.InstallFlags(flags)
	certDir := t.TempDir()
	args := []string{
		"--host=tcp://engine:2376", "--tls=true", "--tlsverify=false",
		"--tlscacert=" + filepath.Join(certDir, "ca.pem"),
		"--tlscert=" + filepath.Join(certDir, "cert.pem"),
		"--tlskey=" + filepath.Join(certDir, "key.pem"),
	}
	require.NoError(t, flags.Parse(args))
	ctx, err := withDMRDockerConnection(t.Context(), nil, flags)
	require.NoError(t, err)
	cmd := dmrmodels.DockerCommand(ctx, "model", "inspect", "ai/test")
	for _, arg := range args {
		assert.Contains(t, cmd.Args, arg)
	}
}

func TestDMRDockerConnectionErrorNamesEngine(t *testing.T) {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		t.Skip("Desktop engine routing is host-only")
	}
	t.Setenv("MODEL_RUNNER_HOST", "")
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell Docker shim")
	}
	dir := t.TempDir()
	writeMockCLI(t, filepath.Join(dir, "docker"), "#!/bin/sh\necho 'missing engine' >&2\nexit 1\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	opts := cliflags.NewClientOptions()
	flags := pflag.NewFlagSet("docker", pflag.ContinueOnError)
	opts.InstallFlags(flags)
	require.NoError(t, flags.Parse([]string{"--host=unix:///missing-dmr-engine.sock"}))
	ctx, err := withDMRDockerConnection(t.Context(), nil, flags)
	require.NoError(t, err)
	baseURL, client := dmrmodels.ResolveBaseURL(ctx, nil, "http://model-runner.docker.internal/engines/v1/")
	require.NotNil(t, client)
	defer client.CloseIdleConnections()
	// The transport retains its selection even when used with a different context.
	_, err = dmrmodels.ListModelsAt(context.WithoutCancel(t.Context()), client, baseURL)
	require.ErrorContains(t, err, "unix:///missing-dmr-engine.sock")
}

func TestDMRSelectedDockerStatusFailureDoesNotFallBack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell Docker shim")
	}
	dir := t.TempDir()
	writeMockCLI(t, filepath.Join(dir, "docker"), "#!/bin/sh\necho 'selected engine unavailable' >&2\nexit 1\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MODEL_RUNNER_HOST", "")
	opts := cliflags.NewClientOptions()
	flags := pflag.NewFlagSet("docker", pflag.ContinueOnError)
	opts.InstallFlags(flags)
	require.NoError(t, flags.Parse([]string{"--context=unavailable"}))
	ctx, err := withDMRDockerConnection(t.Context(), nil, flags)
	require.NoError(t, err)

	_, err = dmrmodels.ListModels(ctx)
	require.ErrorContains(t, err, "selected engine unavailable")
	require.ErrorContains(t, err, "--context=unavailable")
	_, err = dmr.NewClient(ctx, &latest.ModelConfig{Provider: "dmr", Model: "ai/test"})
	require.ErrorContains(t, err, "selected engine unavailable")
}

// TestDMRDockerHelper emulates Docker's stdio tunnel using the real context resolver.
func TestDMRDockerHelper(t *testing.T) {
	if os.Getenv("DMR_DOCKER_HELPER") != "1" {
		return
	}
	var args []string
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	opts := cliflags.NewClientOptions()
	flags := pflag.NewFlagSet("docker", pflag.ContinueOnError)
	opts.InstallFlags(flags)
	flags.SetInterspersed(false)
	require.NoError(t, flags.Parse(args))
	opts.SetDefaultOptions(flags)
	dockerconfig.SetDir(opts.ConfigDir)
	cfg, err := dockerconfig.Load(dockerconfig.Dir())
	require.NoError(t, err)
	client, err := command.NewAPIClientFromFlags(opts, cfg)
	require.NoError(t, err)
	commandArgs := flags.Args()
	if len(commandArgs) > 1 && commandArgs[0] == "system" && commandArgs[1] == "dial-stdio" {
		conn, err := client.Dialer()(t.Context())
		require.NoError(t, err)
		go func() { _, _ = io.Copy(conn, os.Stdin); _ = conn.Close() }()
		_, _ = io.Copy(os.Stdout, conn)
		os.Exit(0)
	}
	require.NoError(t, os.WriteFile(os.Getenv("DMR_ARGS_FILE"), []byte(strings.Join(args, "\n")+"\n"), 0o600))
	_, _ = os.Stdout.WriteString(`{"endpoint":"http://model-runner.docker.internal/engines/v1/"}`)
	os.Exit(0)
}

func TestDMRDockerTunnelCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell Docker shim")
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		t.Skip("Desktop engine routing is host-only")
	}
	t.Setenv("MODEL_RUNNER_HOST", "")
	dir := t.TempDir()
	// Read forever without answering HTTP, as a stuck engine/TLS handshake would.
	writeMockCLI(t, filepath.Join(dir, "docker"), "#!/bin/sh\nwhile read line; do :; done\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, err := withDMRDockerConnection(t.Context(), nil, nil)
	require.NoError(t, err)
	baseURL, client := dmrmodels.ResolveBaseURL(ctx, nil, "http://model-runner.docker.internal/engines/v1/")
	require.NotNil(t, client)
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	_, err = dmrmodels.ListModelsAt(ctx, client, baseURL)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestDMRDockerConnectionAnchorsPaths(t *testing.T) {
	t.Chdir(t.TempDir())
	original, err := os.Getwd()
	require.NoError(t, err)
	opts := cliflags.NewClientOptions()
	flags := pflag.NewFlagSet("docker", pflag.ContinueOnError)
	opts.InstallFlags(flags)
	require.NoError(t, flags.Parse([]string{
		"--context=desktop-test", "--config=./docker-config", "--tlsverify=true",
		"--tlscacert=./tls/ca.pem", "--tlscert=./tls/cert.pem", "--tlskey=./tls/key.pem",
	}))
	ctx, err := withDMRDockerConnection(t.Context(), nil, flags)
	require.NoError(t, err)
	t.Chdir(t.TempDir())
	args := dmrmodels.DockerCommand(ctx, "model", "status", "--json").Args
	for flag, path := range map[string]string{
		"config": "docker-config", "tlscacert": "tls/ca.pem", "tlscert": "tls/cert.pem", "tlskey": "tls/key.pem",
	} {
		assert.Contains(t, args, "--"+flag+"="+filepath.Join(original, path))
	}
	assert.Contains(t, args, "--context=desktop-test")
}

func TestDMRDockerConnectionTLSDefaults(t *testing.T) {
	t.Chdir(t.TempDir())
	original, err := os.Getwd()
	require.NoError(t, err)
	for _, certExists := range []bool{false, true} {
		t.Run(strconv.FormatBool(certExists), func(t *testing.T) {
			opts := cliflags.NewClientOptions()
			flags := pflag.NewFlagSet("docker", pflag.ContinueOnError)
			opts.InstallFlags(flags)
			// Emulate defaults from a relative DOCKER_CERT_PATH without marking flags changed.
			opts.TLSOptions.CAFile = "ca.pem"
			opts.TLSOptions.CertFile = "cert.pem"
			opts.TLSOptions.KeyFile = "key.pem"
			if certExists {
				require.NoError(t, os.WriteFile("cert.pem", nil, 0o600))
				require.NoError(t, os.WriteFile("key.pem", nil, 0o600))
			}
			require.NoError(t, flags.Parse([]string{"--host=tcp://engine:2376", "--tlsverify=false"}))
			ctx, err := withDMRDockerConnection(t.Context(), nil, flags)
			require.NoError(t, err)
			args := dmrmodels.DockerCommand(ctx, "model", "status", "--json").Args
			assert.Contains(t, args, "--tlscacert="+filepath.Join(original, "ca.pem"))
			for flag, file := range map[string]string{"tlscert": "cert.pem", "tlskey": "key.pem"} {
				want := ""
				if certExists {
					want = filepath.Join(original, file)
				}
				assert.Contains(t, args, "--"+flag+"="+want)
			}
		})
	}
}
