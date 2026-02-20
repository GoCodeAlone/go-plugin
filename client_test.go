// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoCodeAlone/go-plugin/internal/cmdrunner"
	"github.com/GoCodeAlone/go-plugin/runner"
	"github.com/hashicorp/go-hclog"
)

// This tests a bug where Kill would start
func TestClient_killStart(t *testing.T) {
	// Create a temporary dir to store the result file
	td := t.TempDir()
	defer os.RemoveAll(td)

	// Start the client
	path := filepath.Join(td, "booted")
	process := helperProcess("bad-version", path)
	c := NewClient(&ClientConfig{Cmd: process, HandshakeConfig: testHandshake})
	defer c.Kill()

	// Verify our path doesn't exist
	if _, err := os.Stat(path); err == nil || !os.IsNotExist(err) {
		t.Fatalf("bad: %s", err)
	}

	// Test that it parses the proper address
	if _, err := c.Start(); err == nil {
		t.Fatal("expected error")
	}

	// Verify we started
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("bad: %s", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("bad: %s", err)
	}

	// Test that Kill does nothing really
	c.Kill()

	// Test that it knows it is exited
	if !c.Exited() {
		t.Fatal("should say client has exited")
	}

	if !c.killed() {
		t.Fatal("process should have failed")
	}

	// Verify our path doesn't exist
	if _, err := os.Stat(path); err == nil || !os.IsNotExist(err) {
		t.Fatalf("bad: %s", err)
	}
}

func TestClient_noStdoutScannerRace(t *testing.T) {
	t.Parallel()

	process := helperProcess("test-grpc")
	logger := &trackingLogger{Logger: hclog.Default()}
	c := NewClient(&ClientConfig{
		RunnerFunc: func(l hclog.Logger, cmd *exec.Cmd, tmpDir string) (runner.Runner, error) {
			process.Env = append(process.Env, cmd.Env...)
			concreteRunner, err := cmdrunner.NewCmdRunner(l, process)
			if err != nil {
				return nil, err
			}
			// Inject a delay before calling .Read() method on the command's
			// stdout reader. This ensures that if there is a race between the
			// stdout scanner loop reading stdout and runner.Wait() closing
			// stdout, .Wait() will win and trigger a scanner error in the logs.
			return &delayedStdoutCmdRunner{concreteRunner}, nil
		},
		HandshakeConfig:  testHandshake,
		Plugins:          testGRPCPluginMap,
		AllowedProtocols: []Protocol{ProtocolGRPC},
		Logger:           logger,
	})

	// Grab the client so the process starts
	if _, err := c.Client(); err != nil {
		c.Kill()
		t.Fatalf("err: %s", err)
	}

	// Kill it gracefully
	c.Kill()

	assertLines(t, logger.errorLogs, 0)
}

type delayedStdoutCmdRunner struct {
	*cmdrunner.CmdRunner
}

func (m *delayedStdoutCmdRunner) Stdout() io.ReadCloser {
	return &delayedReader{m.CmdRunner.Stdout()}
}

type delayedReader struct {
	io.ReadCloser
}

func (d *delayedReader) Read(p []byte) (n int, err error) {
	time.Sleep(100 * time.Millisecond)
	return d.ReadCloser.Read(p)
}

func TestClient_grpc_servercrash(t *testing.T) {
	process := helperProcess("test-grpc")
	c := NewClient(&ClientConfig{
		Cmd:              process,
		HandshakeConfig:  testHandshake,
		Plugins:          testGRPCPluginMap,
		AllowedProtocols: []Protocol{ProtocolGRPC},
	})
	defer c.Kill()

	if _, err := c.Start(); err != nil {
		t.Fatalf("err: %s", err)
	}

	if v := c.Protocol(); v != ProtocolGRPC {
		t.Fatalf("bad: %s", v)
	}

	// Grab the RPC client
	client, err := c.Client()
	if err != nil {
		t.Fatalf("err should be nil, got %s", err)
	}

	// Grab the impl
	raw, err := client.Dispense("test")
	if err != nil {
		t.Fatalf("err should be nil, got %s", err)
	}

	_, ok := raw.(testInterface)
	if !ok {
		t.Fatalf("bad: %#v", raw)
	}

	c.runner.Kill(context.Background())

	select {
	case <-c.doneCtx.Done():
	case <-time.After(time.Second * 2):
		t.Fatal("Context was not closed")
	}
}

func TestClient_grpc(t *testing.T) {
	process := helperProcess("test-grpc")
	c := NewClient(&ClientConfig{
		Cmd:              process,
		HandshakeConfig:  testHandshake,
		Plugins:          testGRPCPluginMap,
		AllowedProtocols: []Protocol{ProtocolGRPC},
	})
	defer c.Kill()

	if _, err := c.Start(); err != nil {
		t.Fatalf("err: %s", err)
	}

	if v := c.Protocol(); v != ProtocolGRPC {
		t.Fatalf("bad: %s", v)
	}

	// Grab the RPC client
	client, err := c.Client()
	if err != nil {
		t.Fatalf("err should be nil, got %s", err)
	}

	// Grab the impl
	raw, err := client.Dispense("test")
	if err != nil {
		t.Fatalf("err should be nil, got %s", err)
	}

	impl, ok := raw.(testInterface)
	if !ok {
		t.Fatalf("bad: %#v", raw)
	}

	result := impl.Double(21)
	if result != 42 {
		t.Fatalf("bad: %#v", result)
	}

	// Kill it
	c.Kill()

	// Test that it knows it is exited
	if !c.Exited() {
		t.Fatal("should say client has exited")
	}

	if c.killed() {
		t.Fatal("process failed to exit gracefully")
	}
}

func TestClient_cmdAndReattach(t *testing.T) {
	config := &ClientConfig{
		Cmd:      helperProcess("start-timeout"),
		Reattach: &ReattachConfig{},
	}

	c := NewClient(config)
	defer c.Kill()

	_, err := c.Start()
	if err == nil {
		t.Fatal("err should not be nil")
	}
}

func TestClient_reattachGRPC(t *testing.T) {
	for name, tc := range map[string]struct {
		useReattachFunc bool
	}{
		"default":          {false},
		"use ReattachFunc": {true},
	} {
		t.Run(name, func(t *testing.T) {
			testClient_reattachGRPC(t, tc.useReattachFunc)
		})
	}
}

func testClient_reattachGRPC(t *testing.T, useReattachFunc bool) {
	process := helperProcess("test-grpc")
	c := NewClient(&ClientConfig{
		Cmd:              process,
		HandshakeConfig:  testHandshake,
		Plugins:          testGRPCPluginMap,
		AllowedProtocols: []Protocol{ProtocolGRPC},
	})
	defer c.Kill()

	// Grab the RPC client
	_, err := c.Client()
	if err != nil {
		t.Fatalf("err should be nil, got %s", err)
	}

	// Get the reattach configuration
	reattach := c.ReattachConfig()

	if useReattachFunc {
		pid := reattach.Pid
		reattach.Pid = 0
		reattach.ReattachFunc = cmdrunner.ReattachFunc(pid, reattach.Addr)
	}

	// Create a new client
	c = NewClient(&ClientConfig{
		Reattach:         reattach,
		HandshakeConfig:  testHandshake,
		Plugins:          testGRPCPluginMap,
		AllowedProtocols: []Protocol{ProtocolGRPC},
	})

	// Grab the RPC client
	client, err := c.Client()
	if err != nil {
		t.Fatalf("err should be nil, got %s", err)
	}

	// Grab the impl
	raw, err := client.Dispense("test")
	if err != nil {
		t.Fatalf("err should be nil, got %s", err)
	}

	impl, ok := raw.(testInterface)
	if !ok {
		t.Fatalf("bad: %#v", raw)
	}

	result := impl.Double(21)
	if result != 42 {
		t.Fatalf("bad: %#v", result)
	}

	// Kill it
	c.Kill()

	// Test that it knows it is exited
	if !c.Exited() {
		t.Fatal("should say client has exited")
	}

	if c.killed() {
		t.Fatal("process failed to exit gracefully")
	}
}

func TestClient_RequestGRPCMultiplexing_UnsupportedByPlugin(t *testing.T) {
	for _, name := range []string{
		"mux-grpc-with-old-plugin",
		"mux-grpc-with-unsupported-plugin",
	} {
		t.Run(name, func(t *testing.T) {
			process := helperProcess(name)
			c := NewClient(&ClientConfig{
				Cmd:                 process,
				HandshakeConfig:     testHandshake,
				Plugins:             testGRPCPluginMap,
				AllowedProtocols:    []Protocol{ProtocolGRPC},
				GRPCBrokerMultiplex: true,
			})
			defer c.Kill()

			_, err := c.Start()
			if err == nil {
				t.Fatal("expected error")
			}

			if !errors.Is(err, ErrGRPCBrokerMuxNotSupported) {
				t.Fatalf("expected %s, but got %s", ErrGRPCBrokerMuxNotSupported, err)
			}
		})
	}
}

func TestClient_TLS_grpc(t *testing.T) {
	// Add TLS config to client
	tlsConfig, err := helperTLSProvider()
	if err != nil {
		t.Fatalf("err should be nil, got %s", err)
	}

	process := helperProcess("test-grpc-tls")
	c := NewClient(&ClientConfig{
		Cmd:              process,
		HandshakeConfig:  testHandshake,
		Plugins:          testGRPCPluginMap,
		TLSConfig:        tlsConfig,
		AllowedProtocols: []Protocol{ProtocolGRPC},
	})
	defer c.Kill()

	// Grab the RPC client
	client, err := c.Client()
	if err != nil {
		t.Fatalf("err should be nil, got %s", err)
	}

	// Grab the impl
	raw, err := client.Dispense("test")
	if err != nil {
		t.Fatalf("err should be nil, got %s", err)
	}

	impl, ok := raw.(testInterface)
	if !ok {
		t.Fatalf("bad: %#v", raw)
	}

	result := impl.Double(21)
	if result != 42 {
		t.Fatalf("bad: %#v", result)
	}

	// Kill it
	c.Kill()

	if !c.Exited() {
		t.Fatal("should say client has exited")
	}

	if c.killed() {
		t.Fatal("process failed to exit gracefully")
	}
}

func TestClient_secureConfigAndReattach(t *testing.T) {
	config := &ClientConfig{
		SecureConfig: &SecureConfig{},
		Reattach:     &ReattachConfig{},
	}

	c := NewClient(config)
	defer c.Kill()

	_, err := c.Start()
	if err != ErrSecureConfigAndReattach {
		t.Fatalf("err should not be %s, got %s", ErrSecureConfigAndReattach, err)
	}
}

func TestClient_wrongVersion(t *testing.T) {
	process := helperProcess("test-proto-upgraded-plugin")
	c := NewClient(&ClientConfig{
		Cmd:              process,
		HandshakeConfig:  testHandshake,
		Plugins:          testGRPCPluginMap,
		AllowedProtocols: []Protocol{ProtocolGRPC},
	})
	defer c.Kill()

	// Get the client
	_, err := c.Client()
	if err == nil {
		t.Fatal("expected incorrect protocol version server")
	}

}

func TestClient_legacyServer(t *testing.T) {
	// test using versioned plugins version when the server supports only
	// supports one
	process := helperProcess("test-proto-upgraded-client")
	c := NewClient(&ClientConfig{
		Cmd:             process,
		HandshakeConfig: testVersionedHandshake,
		VersionedPlugins: map[int]PluginSet{
			2: testGRPCPluginMap,
		},
		AllowedProtocols: []Protocol{ProtocolGRPC},
	})
	defer c.Kill()

	// Get the client
	client, err := c.Client()
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	if c.NegotiatedVersion() != 2 {
		t.Fatal("using incorrect version", c.NegotiatedVersion())
	}

	// Ping, should work
	if err := client.Ping(); err == nil {
		t.Fatal("expected error, should negotiate wrong plugin")
	}
}

func TestClient_versionedClient(t *testing.T) {
	process := helperProcess("test-versioned-plugins")
	c := NewClient(&ClientConfig{
		Cmd:             process,
		HandshakeConfig: testVersionedHandshake,
		VersionedPlugins: map[int]PluginSet{
			2: testGRPCPluginMap,
		},
		AllowedProtocols: []Protocol{ProtocolGRPC},
	})
	defer c.Kill()

	if _, err := c.Start(); err != nil {
		t.Fatalf("err: %s", err)
	}

	if v := c.Protocol(); v != ProtocolGRPC {
		t.Fatalf("bad: %s", v)
	}

	// Grab the RPC client
	client, err := c.Client()
	if err != nil {
		t.Fatalf("err should be nil, got %s", err)
	}

	if c.NegotiatedVersion() != 2 {
		t.Fatal("using incorrect version", c.NegotiatedVersion())
	}

	// Grab the impl
	raw, err := client.Dispense("test")
	if err != nil {
		t.Fatalf("err should be nil, got %s", err)
	}

	_, ok := raw.(testInterface)
	if !ok {
		t.Fatalf("bad: %#v", raw)
	}

	c.runner.Kill(context.Background())

	select {
	case <-c.doneCtx.Done():
	case <-time.After(time.Second * 2):
		t.Fatal("Context was not closed")
	}
}

func TestClient_mtlsClient(t *testing.T) {
	process := helperProcess("test-mtls")
	c := NewClient(&ClientConfig{
		AutoMTLS:        true,
		Cmd:             process,
		HandshakeConfig: testVersionedHandshake,
		VersionedPlugins: map[int]PluginSet{
			2: testGRPCPluginMap,
		},
		AllowedProtocols: []Protocol{ProtocolGRPC},
	})
	defer c.Kill()

	if _, err := c.Start(); err != nil {
		t.Fatalf("err: %s", err)
	}

	if v := c.Protocol(); v != ProtocolGRPC {
		t.Fatalf("bad: %s", v)
	}

	// Grab the RPC client
	client, err := c.Client()
	if err != nil {
		t.Fatalf("err should be nil, got %s", err)
	}

	if c.NegotiatedVersion() != 2 {
		t.Fatal("using incorrect version", c.NegotiatedVersion())
	}

	// Grab the impl
	raw, err := client.Dispense("test")
	if err != nil {
		t.Fatalf("err should be nil, got %s", err)
	}

	tester, ok := raw.(testInterface)
	if !ok {
		t.Fatalf("bad: %#v", raw)
	}

	n := tester.Double(3)
	if n != 6 {
		t.Fatal("invalid response", n)
	}

	c.runner.Kill(context.Background())

	select {
	case <-c.doneCtx.Done():
	case <-time.After(time.Second * 2):
		t.Fatal("Context was not closed")
	}
}

func TestClient_logger(t *testing.T) {
	t.Run("grpc", func(t *testing.T) { testClient_logger(t, "grpc") })
}

func testClient_logger(t *testing.T, proto string) {
	var buffer bytes.Buffer
	mutex := new(sync.Mutex)
	stderr := io.MultiWriter(os.Stderr, &buffer)
	// Custom hclog.Logger
	clientLogger := hclog.New(&hclog.LoggerOptions{
		Name:   "test-logger",
		Level:  hclog.Trace,
		Output: stderr,
		Mutex:  mutex,
	})

	process := helperProcess("test-interface-logger-" + proto)
	c := NewClient(&ClientConfig{
		Cmd:              process,
		HandshakeConfig:  testHandshake,
		Plugins:          testGRPCPluginMap,
		Logger:           clientLogger,
		AllowedProtocols: []Protocol{ProtocolGRPC},
	})
	defer c.Kill()

	// Grab the RPC client
	client, err := c.Client()
	if err != nil {
		t.Fatalf("err should be nil, got %s", err)
	}

	// Grab the impl
	raw, err := client.Dispense("test")
	if err != nil {
		t.Fatalf("err should be nil, got %s", err)
	}

	impl, ok := raw.(testInterface)
	if !ok {
		t.Fatalf("bad: %#v", raw)
	}

	{
		// Discard everything else, and capture the output we care about
		mutex.Lock()
		buffer.Reset()
		mutex.Unlock()
		impl.PrintKV("foo", "bar")
		time.Sleep(100 * time.Millisecond)
		mutex.Lock()
		output := buffer.String()
		mutex.Unlock()
		if !strings.Contains(output, "foo=bar") {
			t.Fatalf("bad: %q", output)
		}
	}

	{
		// Try an integer type
		mutex.Lock()
		buffer.Reset()
		mutex.Unlock()
		impl.PrintKV("foo", 12)
		time.Sleep(100 * time.Millisecond)
		mutex.Lock()
		output := buffer.String()
		mutex.Unlock()
		if !strings.Contains(output, "foo=12") {
			t.Fatalf("bad: %q", output)
		}
	}

	// Kill it
	c.Kill()

	// Test that it knows it is exited
	if !c.Exited() {
		t.Fatal("should say client has exited")
	}

	if c.killed() {
		t.Fatal("process failed to exit gracefully")
	}
}

// Test that we continue to consume stderr over long lines.
func TestClient_logStderr(t *testing.T) {
	stderr := bytes.Buffer{}
	c := NewClient(&ClientConfig{
		Stderr: &stderr,
		Cmd: &exec.Cmd{
			Path: "test",
		},
		PluginLogBufferSize: 32,
	})
	c.clientWaitGroup.Add(1)

	msg := `
this line is more than 32 bytes long
and this line is more than 32 bytes long
{"a": "b", "@level": "debug"}
this line is short
`

	reader := strings.NewReader(msg)

	c.pipesWaitGroup.Add(1)
	c.logStderr(c.config.Cmd.Path, reader)
	read := stderr.String()

	if read != msg {
		t.Fatalf("\nexpected output: %q\ngot output:      %q", msg, read)
	}
}

func TestClient_logStderrParseJSON(t *testing.T) {
	logBuf := bytes.Buffer{}
	c := NewClient(&ClientConfig{
		Stderr:              bytes.NewBuffer(nil),
		Cmd:                 &exec.Cmd{Path: "test"},
		PluginLogBufferSize: 64,
		Logger: hclog.New(&hclog.LoggerOptions{
			Name:       "test-logger",
			Level:      hclog.Trace,
			Output:     &logBuf,
			JSONFormat: true,
		}),
	})
	c.clientWaitGroup.Add(1)

	msg := `{"@message": "this is a message", "@level": "info"}
{"@message": "this is a large message that is more than 64 bytes long", "@level": "info"}`
	reader := strings.NewReader(msg)

	c.pipesWaitGroup.Add(1)
	c.logStderr(c.config.Cmd.Path, reader)
	logs := strings.Split(strings.TrimSpace(logBuf.String()), "\n")

	wants := []struct {
		wantLevel   string
		wantMessage string
	}{
		{"info", "this is a message"},
		{"debug", `{"@message": "this is a large message that is more than 64 bytes`},
		{"debug", ` long", "@level": "info"}`},
	}

	if len(logs) != len(wants) {
		t.Fatalf("expected %d logs, got %d", len(wants), len(logs))
	}

	for i, tt := range wants {
		l := make(map[string]interface{})
		if err := json.Unmarshal([]byte(logs[i]), &l); err != nil {
			t.Fatal(err)
		}

		if l["@level"] != tt.wantLevel {
			t.Fatalf("expected level %q, got %q", tt.wantLevel, l["@level"])
		}

		if l["@message"] != tt.wantMessage {
			t.Fatalf("expected message %q, got %q", tt.wantMessage, l["@message"])
		}
	}
}

type trackingLogger struct {
	hclog.Logger
	errorLogs []string
}

func (l *trackingLogger) Error(msg string, args ...interface{}) {
	l.errorLogs = append(l.errorLogs, fmt.Sprintf("%s: %v", msg, args))
	l.Logger.Error(msg, args...)
}

func assertLines(t *testing.T, lines []string, expected int) {
	t.Helper()
	if len(lines) != expected {
		t.Errorf("expected %d, got %d", expected, len(lines))
		for _, log := range lines {
			t.Error(log)
		}
	}
}
