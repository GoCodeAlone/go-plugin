// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"bytes"
	"context"
	"net"

	"github.com/GoCodeAlone/go-plugin/internal/grpcmux"
	hclog "github.com/hashicorp/go-hclog"
	"github.com/mitchellh/go-testing-interface"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// The testing file contains test helpers that you can use outside of
// this package for making it easier to test plugins themselves.

// TestConn is a helper function for returning a client and server
// net.Conn connected to each other.
func TestConn(t testing.T) (net.Conn, net.Conn) {
	// Listen to any local port. This listener will be closed
	// after a single connection is established.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	// Start a goroutine to accept our client connection
	var serverConn net.Conn
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		defer l.Close()
		var err error
		serverConn, err = l.Accept()
		if err != nil {
			t.Fatalf("err: %s", err)
		}
	}()

	// Connect to the server
	clientConn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	// Wait for the server side to acknowledge it has connected
	<-doneCh

	return clientConn, serverConn
}

// TestGRPCConn returns a gRPC client conn and grpc server that are connected
// together and configured. The register function is used to register services
// prior to the Serve call. This is used to test gRPC connections.
func TestGRPCConn(t testing.T, register func(*grpc.Server)) (*grpc.ClientConn, *grpc.Server) {
	// Create a listener
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	server := grpc.NewServer()
	register(server)
	go server.Serve(l)

	// Connect to the server using passthrough resolver to avoid DNS resolution
	// on the raw address.
	conn, err := grpc.NewClient(
		"passthrough:///"+l.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	return conn, server
}

// TestPluginGRPCConn returns a plugin gRPC client and server that are connected
// together and configured. This is used to test gRPC connections.
func TestPluginGRPCConn(t testing.T, multiplex bool, ps map[string]Plugin) (*GRPCClient, *GRPCServer) {
	// Create a listener
	ln, err := serverListener(UnixSocketConfig{})
	if err != nil {
		t.Fatal(err)
	}

	logger := hclog.New(&hclog.LoggerOptions{
		Level: hclog.Debug,
	})

	// Start up the server
	var muxer *grpcmux.GRPCServerMuxer
	if multiplex {
		muxer = grpcmux.NewGRPCServerMuxer(logger, ln)
		ln = muxer
	}
	server := &GRPCServer{
		Plugins: ps,
		DoneCh:  make(chan struct{}),
		Server:  DefaultGRPCServer,
		Stdout:  new(bytes.Buffer),
		Stderr:  new(bytes.Buffer),
		logger:  logger,
		muxer:   muxer,
	}
	if err := server.Init(); err != nil {
		t.Fatalf("err: %s", err)
	}
	go server.Serve(ln)

	client := &Client{
		address:  ln.Addr(),
		protocol: ProtocolGRPC,
		config: &ClientConfig{
			Plugins:             ps,
			GRPCBrokerMultiplex: multiplex,
		},
		logger: logger,
	}

	grpcClient, err := newGRPCClient(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}

	return grpcClient, server
}
